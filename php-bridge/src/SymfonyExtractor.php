<?php

declare(strict_types=1);

namespace RagCode\Bridge;

use PhpParser\Node;
use PhpParser\NodeVisitorAbstract;
use PhpParser\Node\Stmt;
use PhpParser\Node\Attribute;
use PhpParser\Node\AttributeGroup;

/**
 * Symfony-specific AST visitor that extracts framework metadata.
 *
 * Detects and enriches:
 * - Controllers (AbstractController, #[Route])
 * - Entities (Doctrine ORM attributes)
 * - Repositories (ServiceEntityRepository)
 * - Event listeners (#[AsEventListener], EventSubscriberInterface)
 * - Console commands (extends Command)
 * - Voters (extends Voter)
 * - Form types (extends AbstractType)
 * - Service tags (#[AutoconfigureTag], #[AsTaggedItem])
 * - DI config (#[Autowire], constructor injection)
 */
class SymfonyExtractor extends NodeVisitorAbstract
{
    private string $filePath;
    private string $code;
    private string $currentNamespace = '';
    /** @var array<string, string> alias => FQN */
    private array $imports = [];

    /** @var array<string, array<string, mixed>> keyed by FQN */
    private array $metadata = [];

    public function __construct(string $filePath, string $code)
    {
        $this->filePath = $filePath;
        $this->code = $code;
    }

    /**
     * @return array<string, array<string, mixed>>
     */
    public function getMetadata(): array
    {
        return $this->metadata;
    }

    public function enterNode(Node $node): ?int
    {
        if ($node instanceof Stmt\Namespace_) {
            $this->currentNamespace = $node->name ? $node->name->toString() : '';
            $this->imports = [];
            return null;
        }

        if ($node instanceof Stmt\Use_) {
            foreach ($node->uses as $use) {
                $this->imports[$use->getAlias()->toString()] = $use->name->toString();
            }
            return null;
        }

        if ($node instanceof Stmt\GroupUse) {
            $prefix = $node->prefix->toString();
            foreach ($node->uses as $use) {
                $this->imports[$use->getAlias()->toString()] = $prefix . '\\' . $use->name->toString();
            }
            return null;
        }

        if ($node instanceof Stmt\Class_) {
            $this->analyzeClass($node);
            return null;
        }

        return null;
    }

    private function analyzeClass(Stmt\Class_ $node): void
    {
        if ($node->isAnonymous() || $node->name === null) {
            return;
        }

        $name = $node->name->toString();
        $fqn = $this->currentNamespace ? $this->currentNamespace . '\\' . $name : $name;
        $extends = $node->extends ? $this->resolveTypeName($node->extends) : null;
        $implements = array_map(fn($i) => $this->resolveTypeName($i), $node->implements);

        $meta = [];

        // Detect Symfony type by parent class
        if ($extends !== null) {
            $meta = $this->detectByParentClass($extends, $node);
        }

        // Detect by implemented interfaces
        $meta = array_merge($meta, $this->detectByInterfaces($implements, $node));

        // Detect by class attributes
        $meta = array_merge($meta, $this->detectByAttributes($node->attrGroups, $node));

        // Detect Doctrine Entity
        $entityMeta = $this->detectDoctrineEntity($node);
        if ($entityMeta) {
            $meta = array_merge($meta, $entityMeta);
        }

        // Extract route attributes from methods
        $routes = $this->extractRoutes($node);
        if ($routes) {
            $meta['routes'] = $routes;
        }

        // Only extract DI info when the class already has Symfony indicators,
        // otherwise every class with a constructor gets tagged as Symfony.
        if (!empty($meta)) {
            $diInfo = $this->extractDIInfo($node);
            if ($diInfo) {
                $meta['di_dependencies'] = $diInfo;
            }
            $meta['framework'] = 'symfony';
            $this->metadata[$fqn] = $meta;
        }
    }

    private function detectByParentClass(string $extends, Stmt\Class_ $node): array
    {
        $meta = [];
        $shortName = $this->shortClassName($extends);

        $parentMap = [
            'AbstractController' => 'controller',
            'Controller' => 'controller',
            'ServiceEntityRepository' => 'repository',
            'Command' => 'command',
            'Voter' => 'voter',
            'AbstractType' => 'form_type',
            'AbstractExtension' => 'twig_extension',
        ];

        if (isset($parentMap[$shortName])) {
            $meta['symfony_type'] = $parentMap[$shortName];

            // Extract command-specific info
            if ($parentMap[$shortName] === 'command') {
                $meta = array_merge($meta, $this->extractCommandInfo($node));
            }

            // Extract form type info
            if ($parentMap[$shortName] === 'form_type') {
                $meta = array_merge($meta, $this->extractFormTypeInfo($node));
            }

            // Extract repository info
            if ($parentMap[$shortName] === 'repository') {
                $meta = array_merge($meta, $this->extractRepositoryInfo($node));
            }
        }

        return $meta;
    }

    private function detectByInterfaces(array $implements, Stmt\Class_ $node): array
    {
        $meta = [];

        foreach ($implements as $iface) {
            $short = $this->shortClassName($iface);

            if ($short === 'EventSubscriberInterface') {
                $meta['symfony_type'] = $meta['symfony_type'] ?? 'event_subscriber';
                $meta = array_merge($meta, $this->extractSubscribedEvents($node));
            }

            if ($short === 'MessageHandlerInterface') {
                $meta['symfony_type'] = $meta['symfony_type'] ?? 'message_handler';
            }

            if ($short === 'EntityInterface' || $short === 'JsonSerializable') {
                // Not specifically Symfony, but useful metadata
            }
        }

        return $meta;
    }

    private function detectByAttributes(array $attrGroups, Stmt\Class_ $node): array
    {
        $meta = [];

        foreach ($attrGroups as $group) {
            foreach ($group->attrs as $attr) {
                $attrName = $this->resolveAttributeName($attr);
                $short = $this->shortClassName($attrName);

                if ($short === 'AsEventListener') {
                    $meta['symfony_type'] = $meta['symfony_type'] ?? 'event_listener';
                    $meta['listened_events'] = $this->extractAttributeArgs($attr);
                }

                if ($short === 'AutoconfigureTag' || $short === 'AsTaggedItem') {
                    $meta['service_tags'] = $meta['service_tags'] ?? [];
                    $meta['service_tags'][] = $this->extractAttributeArgs($attr);
                }

                if ($short === 'AsController') {
                    $meta['symfony_type'] = $meta['symfony_type'] ?? 'controller';
                }

                if ($short === 'AsCommand') {
                    $meta['symfony_type'] = $meta['symfony_type'] ?? 'command';
                    $args = $this->extractAttributeArgs($attr);
                    if (isset($args['name'])) {
                        $meta['command_name'] = $args['name'];
                    } elseif (isset($args[0])) {
                        $meta['command_name'] = $args[0];
                    }
                }

                if ($short === 'AsMessageHandler') {
                    $meta['symfony_type'] = $meta['symfony_type'] ?? 'message_handler';
                }
            }
        }

        return $meta;
    }

    private function detectDoctrineEntity(Stmt\Class_ $node): ?array
    {
        $meta = [];
        $isEntity = false;

        foreach ($node->attrGroups as $group) {
            foreach ($group->attrs as $attr) {
                $attrName = $this->resolveAttributeName($attr);
                $short = $this->shortClassName($attrName);

                if ($short === 'Entity' && $this->isDoctrineAttribute($attrName)) {
                    $isEntity = true;
                    $args = $this->extractAttributeArgs($attr);
                    if (isset($args['repositoryClass'])) {
                        $meta['repository_class'] = $args['repositoryClass'];
                    }
                }

                if ($short === 'Table' && $this->isDoctrineAttribute($attrName)) {
                    $args = $this->extractAttributeArgs($attr);
                    if (isset($args['name'])) {
                        $meta['table_name'] = $args['name'];
                    } elseif (isset($args[0])) {
                        $meta['table_name'] = $args[0];
                    }
                }
            }
        }

        if (!$isEntity) {
            return null;
        }

        $meta['symfony_type'] = 'entity';

        // Extract Doctrine column mapping from properties
        $columns = [];
        foreach ($node->stmts as $stmt) {
            if ($stmt instanceof Stmt\Property) {
                $colInfo = $this->extractDoctrineColumnInfo($stmt);
                if ($colInfo) {
                    $columns[] = $colInfo;
                }
            }
        }
        if ($columns) {
            $meta['doctrine_mapping'] = $columns;
        }

        return $meta;
    }

    private function extractDoctrineColumnInfo(Stmt\Property $prop): ?array
    {
        $info = null;

        foreach ($prop->attrGroups as $group) {
            foreach ($group->attrs as $attr) {
                $attrName = $this->resolveAttributeName($attr);
                $short = $this->shortClassName($attrName);

                if (!$this->isDoctrineAttribute($attrName)) {
                    continue;
                }

                $propName = $prop->props[0]->name->toString();
                $args = $this->extractAttributeArgs($attr);

                if ($short === 'Column') {
                    $info = $info ?? ['name' => $propName];
                    $info['column_type'] = $args['type'] ?? $args[0] ?? null;
                    if (isset($args['length'])) $info['length'] = $args['length'];
                    if (isset($args['nullable'])) $info['nullable'] = $args['nullable'];
                }

                if ($short === 'Id') {
                    $info = $info ?? ['name' => $propName];
                    $info['is_id'] = true;
                }

                if (in_array($short, ['OneToMany', 'ManyToOne', 'OneToOne', 'ManyToMany'])) {
                    $info = $info ?? ['name' => $propName];
                    $info['relation'] = $short;
                    $info['target_entity'] = $args['targetEntity'] ?? $args[0] ?? null;
                    if (isset($args['mappedBy'])) $info['mapped_by'] = $args['mappedBy'];
                    if (isset($args['inversedBy'])) $info['inversed_by'] = $args['inversedBy'];
                }

                if ($short === 'GeneratedValue') {
                    $info = $info ?? ['name' => $propName];
                    $info['generated'] = true;
                }
            }
        }

        return $info;
    }

    /**
     * Extract #[Route] attributes from class and methods.
     */
    private function extractRoutes(Stmt\Class_ $node): array
    {
        $routes = [];

        // Class-level route prefix
        $classRoute = null;
        foreach ($node->attrGroups as $group) {
            foreach ($group->attrs as $attr) {
                $short = $this->shortClassName($this->resolveAttributeName($attr));
                if ($short === 'Route') {
                    $classRoute = $this->extractAttributeArgs($attr);
                }
            }
        }

        // Method-level routes
        foreach ($node->stmts as $stmt) {
            if (!$stmt instanceof Stmt\ClassMethod) {
                continue;
            }

            foreach ($stmt->attrGroups as $group) {
                foreach ($group->attrs as $attr) {
                    $short = $this->shortClassName($this->resolveAttributeName($attr));
                    if ($short === 'Route') {
                        $args = $this->extractAttributeArgs($attr);
                        $route = [
                            'method_name' => $stmt->name->toString(),
                            'path' => $args['path'] ?? $args[0] ?? null,
                            'name' => $args['name'] ?? null,
                            'methods' => $args['methods'] ?? null,
                        ];

                        if ($classRoute) {
                            $prefix = $classRoute['path'] ?? $classRoute[0] ?? '';
                            if ($prefix && $route['path']) {
                                $route['full_path'] = rtrim($prefix, '/') . '/' . ltrim($route['path'], '/');
                            }
                        }

                        $routes[] = $route;
                    }
                }
            }
        }

        return $routes;
    }

    private function extractDIInfo(Stmt\Class_ $node): ?array
    {
        foreach ($node->stmts as $stmt) {
            if ($stmt instanceof Stmt\ClassMethod && $stmt->name->toString() === '__construct') {
                $deps = [];
                foreach ($stmt->params as $param) {
                    $dep = [
                        'name' => ($param->var instanceof Node\Expr\Variable && is_string($param->var->name))
                            ? $param->var->name
                            : '',
                        'type' => $param->type ? $this->printType($param->type) : null,
                    ];

                    // Check for #[Autowire] attribute
                    foreach ($param->attrGroups as $group) {
                        foreach ($group->attrs as $attr) {
                            $short = $this->shortClassName($this->resolveAttributeName($attr));
                            if ($short === 'Autowire') {
                                $dep['autowire'] = $this->extractAttributeArgs($attr);
                            }
                        }
                    }

                    $deps[] = $dep;
                }
                return $deps;
            }
        }
        return null;
    }

    private function extractCommandInfo(Stmt\Class_ $node): array
    {
        $info = [];

        // Check #[AsCommand] attribute
        foreach ($node->attrGroups as $group) {
            foreach ($group->attrs as $attr) {
                $short = $this->shortClassName($this->resolveAttributeName($attr));
                if ($short === 'AsCommand') {
                    $args = $this->extractAttributeArgs($attr);
                    $info['command_name'] = $args['name'] ?? $args[0] ?? null;
                    $info['command_description'] = $args['description'] ?? $args[1] ?? null;
                }
            }
        }

        // Check $defaultName property
        foreach ($node->stmts as $stmt) {
            if ($stmt instanceof Stmt\Property && $stmt->isStatic()) {
                foreach ($stmt->props as $prop) {
                    if ($prop->name->toString() === 'defaultName' && $prop->default) {
                        if ($prop->default instanceof Node\Scalar\String_) {
                            $info['command_name'] = $info['command_name'] ?? $prop->default->value;
                        }
                    }
                }
            }
        }

        return $info;
    }

    private function extractFormTypeInfo(Stmt\Class_ $node): array
    {
        $info = [];
        $fields = [];

        foreach ($node->stmts as $stmt) {
            if ($stmt instanceof Stmt\ClassMethod && $stmt->name->toString() === 'buildForm') {
                // Simple heuristic: look for $builder->add() calls
                // This is a basic extraction - full analysis would need expression walking
                $info['has_build_form'] = true;
            }
            if ($stmt instanceof Stmt\ClassMethod && $stmt->name->toString() === 'configureOptions') {
                $info['has_configure_options'] = true;
            }
        }

        return $info;
    }

    private function extractRepositoryInfo(Stmt\Class_ $node): array
    {
        $info = [];
        $queryMethods = [];

        foreach ($node->stmts as $stmt) {
            if ($stmt instanceof Stmt\ClassMethod) {
                $name = $stmt->name->toString();
                if ($name === '__construct') continue;

                // Any non-inherited method is a custom query method
                if (str_starts_with($name, 'find') || str_starts_with($name, 'get')
                    || str_starts_with($name, 'count') || str_starts_with($name, 'search')) {
                    $queryMethods[] = $name;
                }
            }
        }

        if ($queryMethods) {
            $info['query_methods'] = $queryMethods;
        }

        // Try to detect entity class from constructor
        foreach ($node->stmts as $stmt) {
            if ($stmt instanceof Stmt\ClassMethod && $stmt->name->toString() === '__construct') {
                // Look for parent::__construct($registry, Entity::class) pattern
                if ($stmt->stmts) {
                    foreach ($stmt->stmts as $bodyStmt) {
                        if ($bodyStmt instanceof Stmt\Expression
                            && $bodyStmt->expr instanceof Node\Expr\StaticCall
                        ) {
                            $call = $bodyStmt->expr;
                            if (count($call->args) >= 2) {
                                $secondArg = $call->args[1]->value ?? null;
                                if ($secondArg instanceof Node\Expr\ClassConstFetch) {
                                    $info['entity_class'] = $this->printType($secondArg->class);
                                }
                            }
                        }
                    }
                }
            }
        }

        return $info;
    }

    private function extractSubscribedEvents(Stmt\Class_ $node): array
    {
        $info = [];

        foreach ($node->stmts as $stmt) {
            if ($stmt instanceof Stmt\ClassMethod
                && $stmt->name->toString() === 'getSubscribedEvents'
                && $stmt->isStatic()
            ) {
                $info['is_event_subscriber'] = true;
                // The actual events would require runtime evaluation, skip for now
            }
        }

        return $info;
    }

    // --- Utility methods ---

    private function resolveAttributeName(Attribute $attr): string
    {
        return $this->resolveTypeName($attr->name);
    }

    private function resolveTypeName(Node\Name $name): string
    {
        if ($name->isFullyQualified()) {
            return ltrim($name->toString(), '\\');
        }

        $first = $name->getFirst();
        if (isset($this->imports[$first])) {
            $rest = $name->slice(1);
            if ($rest !== null) {
                return $this->imports[$first] . '\\' . $rest->toString();
            }
            return $this->imports[$first];
        }

        if ($this->currentNamespace) {
            return $this->currentNamespace . '\\' . $name->toString();
        }

        return $name->toString();
    }

    private function printType(Node $type): string
    {
        if ($type instanceof Node\Name) {
            return $this->resolveTypeName($type);
        }
        if ($type instanceof Node\Identifier) {
            return $type->toString();
        }
        if ($type instanceof Node\NullableType) {
            return '?' . $this->printType($type->type);
        }
        if ($type instanceof Node\UnionType) {
            return implode('|', array_map(fn($t) => $this->printType($t), $type->types));
        }
        if ($type instanceof Node\IntersectionType) {
            return implode('&', array_map(fn($t) => $this->printType($t), $type->types));
        }
        return '';
    }

    private function shortClassName(string $fqn): string
    {
        $parts = explode('\\', $fqn);
        return end($parts);
    }

    private function isDoctrineAttribute(string $attrName): bool
    {
        return str_contains($attrName, 'Doctrine\\ORM')
            || str_contains($attrName, 'ORM\\')
            || in_array($this->shortClassName($attrName), [
                'Entity', 'Table', 'Column', 'Id', 'GeneratedValue',
                'OneToMany', 'ManyToOne', 'OneToOne', 'ManyToMany',
                'JoinColumn', 'JoinTable', 'Index', 'UniqueConstraint',
            ]);
    }

    private function extractAttributeArgs(Attribute $attr): array
    {
        $args = [];
        foreach ($attr->args as $i => $arg) {
            $value = $this->extractArgValue($arg->value);
            if ($arg->name !== null) {
                $args[$arg->name->toString()] = $value;
            } else {
                $args[$i] = $value;
            }
        }
        return $args;
    }

    private function extractArgValue(Node\Expr $expr): mixed
    {
        if ($expr instanceof Node\Scalar\String_) {
            return $expr->value;
        }
        if ($expr instanceof Node\Scalar\Int_) {
            return $expr->value;
        }
        if ($expr instanceof Node\Scalar\Float_) {
            return $expr->value;
        }
        if ($expr instanceof Node\Expr\ConstFetch) {
            $name = strtolower($expr->name->toString());
            return match ($name) {
                'true' => true,
                'false' => false,
                'null' => null,
                default => $expr->name->toString(),
            };
        }
        if ($expr instanceof Node\Expr\ClassConstFetch) {
            return $this->printType($expr->class) . '::' . $expr->name->toString();
        }
        if ($expr instanceof Node\Expr\Array_) {
            $result = [];
            foreach ($expr->items as $item) {
                if ($item === null) continue;
                $val = $this->extractArgValue($item->value);
                if ($item->key !== null) {
                    $key = $this->extractArgValue($item->key);
                    $result[$key] = $val;
                } else {
                    $result[] = $val;
                }
            }
            return $result;
        }
        return null;
    }
}
