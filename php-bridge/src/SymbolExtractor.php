<?php

declare(strict_types=1);

namespace RagCode\Bridge;

use PhpParser\Node;
use PhpParser\NodeVisitorAbstract;
use PhpParser\Node\Stmt;
use PhpParser\Node\Expr;
use PhpParser\PrettyPrinter\Standard as PrettyPrinter;

/**
 * AST visitor that extracts PHP symbols (classes, methods, properties, etc.)
 * and outputs them as structured data for the Go bridge.
 *
 * Supports PHP 8.1-8.4: enums, readonly, intersection types, DNF types,
 * typed constants, property hooks, asymmetric visibility.
 */
class SymbolExtractor extends NodeVisitorAbstract
{
    private string $filePath;
    private string $code;
    /** @var string[] */
    private array $lines;
    private string $currentNamespace = '';
    /** @var array<string, string> alias => FQN */
    private array $imports = [];
    private ?string $currentClassName = null;
    private ?string $currentClassType = null; // class, interface, trait, enum

    /** @var array<int, array<string, mixed>> */
    private array $symbols = [];

    private PrettyPrinter $printer;

    public function __construct(string $filePath, string $code)
    {
        $this->filePath = $filePath;
        $this->code = $code;
        $this->lines = explode("\n", $code);
        $this->printer = new PrettyPrinter();
    }

    /**
     * @return array<int, array<string, mixed>>
     */
    public function getSymbols(): array
    {
        return $this->symbols;
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
                $alias = $use->getAlias()->toString();
                $this->imports[$alias] = $use->name->toString();
            }
            return null;
        }

        if ($node instanceof Stmt\GroupUse) {
            $prefix = $node->prefix->toString();
            foreach ($node->uses as $use) {
                $alias = $use->getAlias()->toString();
                $this->imports[$alias] = $prefix . '\\' . $use->name->toString();
            }
            return null;
        }

        if ($node instanceof Stmt\Class_) {
            $this->processClass($node);
            return null;
        }

        if ($node instanceof Stmt\Interface_) {
            $this->processInterface($node);
            return null;
        }

        if ($node instanceof Stmt\Trait_) {
            $this->processTrait($node);
            return null;
        }

        if ($node instanceof Stmt\Enum_) {
            $this->processEnum($node);
            return null;
        }

        if ($node instanceof Stmt\Function_) {
            $this->processFunction($node);
            return null;
        }

        return null;
    }

    public function leaveNode(Node $node): void
    {
        if ($node instanceof Stmt\Class_
            || $node instanceof Stmt\Interface_
            || $node instanceof Stmt\Trait_
            || $node instanceof Stmt\Enum_
        ) {
            $this->currentClassName = null;
            $this->currentClassType = null;
        }
    }

    private function processClass(Stmt\Class_ $node): void
    {
        if ($node->isAnonymous()) {
            return;
        }

        $name = $node->name->toString();
        $this->currentClassName = $name;
        $this->currentClassType = 'class';

        $extends = $node->extends ? $this->resolveTypeName($node->extends) : null;
        $implements = [];
        foreach ($node->implements as $iface) {
            $implements[] = $this->resolveTypeName($iface);
        }

        $uses = [];
        foreach ($node->stmts as $stmt) {
            if ($stmt instanceof Stmt\TraitUse) {
                foreach ($stmt->traits as $trait) {
                    $uses[] = $this->resolveTypeName($trait);
                }
            }
        }

        $modifiers = [
            'is_abstract' => $node->isAbstract(),
            'is_final' => $node->isFinal(),
            'is_readonly' => $node->isReadonly(),
        ];

        $this->symbols[] = [
            'name' => $name,
            'type' => 'class',
            'namespace' => $this->currentNamespace,
            'signature' => $this->buildClassSignature($name, $extends, $implements, $modifiers),
            'file_path' => $this->filePath,
            'start_line' => $node->getStartLine(),
            'end_line' => $node->getEndLine(),
            'code' => $this->extractCode($node->getStartLine(), $node->getEndLine(), 50),
            'docstring' => $this->extractDocComment($node),
            'extends' => $extends,
            'implements' => $implements,
            'uses' => $uses,
            'modifiers' => $modifiers,
        ];

        // Process class members
        foreach ($node->stmts as $stmt) {
            $this->processClassMember($stmt, $name);
        }
    }

    private function processInterface(Stmt\Interface_ $node): void
    {
        $name = $node->name->toString();
        $this->currentClassName = $name;
        $this->currentClassType = 'interface';

        $extends = [];
        foreach ($node->extends as $ext) {
            $extends[] = $this->resolveTypeName($ext);
        }

        $this->symbols[] = [
            'name' => $name,
            'type' => 'interface',
            'namespace' => $this->currentNamespace,
            'signature' => 'interface ' . $name . ($extends ? ' extends ' . implode(', ', $extends) : ''),
            'file_path' => $this->filePath,
            'start_line' => $node->getStartLine(),
            'end_line' => $node->getEndLine(),
            'code' => $this->extractCode($node->getStartLine(), $node->getEndLine(), 50),
            'docstring' => $this->extractDocComment($node),
            'extends' => $extends,
        ];

        foreach ($node->stmts as $stmt) {
            $this->processClassMember($stmt, $name);
        }
    }

    private function processTrait(Stmt\Trait_ $node): void
    {
        $name = $node->name->toString();
        $this->currentClassName = $name;
        $this->currentClassType = 'trait';

        $this->symbols[] = [
            'name' => $name,
            'type' => 'trait',
            'namespace' => $this->currentNamespace,
            'signature' => 'trait ' . $name,
            'file_path' => $this->filePath,
            'start_line' => $node->getStartLine(),
            'end_line' => $node->getEndLine(),
            'code' => $this->extractCode($node->getStartLine(), $node->getEndLine(), 50),
            'docstring' => $this->extractDocComment($node),
        ];

        foreach ($node->stmts as $stmt) {
            $this->processClassMember($stmt, $name);
        }
    }

    private function processEnum(Stmt\Enum_ $node): void
    {
        $name = $node->name->toString();
        $this->currentClassName = $name;
        $this->currentClassType = 'enum';

        $backedType = $node->scalarType ? $this->printType($node->scalarType) : null;

        $implements = [];
        foreach ($node->implements as $iface) {
            $implements[] = $this->resolveTypeName($iface);
        }

        $cases = [];
        foreach ($node->stmts as $stmt) {
            if ($stmt instanceof Stmt\EnumCase) {
                $caseData = ['name' => $stmt->name->toString()];
                if ($stmt->expr !== null) {
                    $caseData['value'] = $this->extractConstExprValue($stmt->expr);
                }
                $cases[] = $caseData;
            }
        }

        $sig = 'enum ' . $name;
        if ($backedType) {
            $sig .= ': ' . $backedType;
        }
        if ($implements) {
            $sig .= ' implements ' . implode(', ', $implements);
        }

        $this->symbols[] = [
            'name' => $name,
            'type' => 'enum',
            'namespace' => $this->currentNamespace,
            'signature' => $sig,
            'file_path' => $this->filePath,
            'start_line' => $node->getStartLine(),
            'end_line' => $node->getEndLine(),
            'code' => $this->extractCode($node->getStartLine(), $node->getEndLine(), 50),
            'docstring' => $this->extractDocComment($node),
            'backed_type' => $backedType,
            'cases' => $cases,
            'implements' => $implements,
        ];

        // Process enum members (methods, constants)
        foreach ($node->stmts as $stmt) {
            if ($stmt instanceof Stmt\EnumCase) {
                continue; // Already processed
            }
            $this->processClassMember($stmt, $name);
        }
    }

    private function processFunction(Stmt\Function_ $node): void
    {
        $name = $node->name->toString();
        $params = $this->extractParameters($node->params);
        $returnType = $node->returnType ? $this->printType($node->returnType) : null;

        $sig = 'function ' . $name . '(' . $this->buildParamString($params) . ')';
        if ($returnType) {
            $sig .= ': ' . $returnType;
        }

        $this->symbols[] = [
            'name' => $name,
            'type' => 'function',
            'namespace' => $this->currentNamespace,
            'signature' => $sig,
            'file_path' => $this->filePath,
            'start_line' => $node->getStartLine(),
            'end_line' => $node->getEndLine(),
            'code' => $this->extractCode($node->getStartLine(), $node->getEndLine()),
            'docstring' => $this->extractDocComment($node),
            'parameters' => $params,
            'return_type' => $returnType,
        ];
    }

    private function processClassMember(Node $stmt, string $className): void
    {
        if ($stmt instanceof Stmt\ClassMethod) {
            $this->processMethod($stmt, $className);
        } elseif ($stmt instanceof Stmt\Property) {
            $this->processProperty($stmt, $className);
        } elseif ($stmt instanceof Stmt\ClassConst) {
            $this->processClassConstant($stmt, $className);
        }
        // TraitUse is handled in processClass
    }

    private function processMethod(Stmt\ClassMethod $node, string $className): void
    {
        $name = $node->name->toString();
        $visibility = $this->getVisibility($node);
        $params = $this->extractParameters($node->params);
        $returnType = $node->returnType ? $this->printType($node->returnType) : null;

        $sig = $visibility . ' function ' . $name . '(' . $this->buildParamString($params) . ')';
        if ($returnType) {
            $sig .= ': ' . $returnType;
        }

        $this->symbols[] = [
            'name' => $name,
            'type' => 'method',
            'namespace' => $this->currentNamespace,
            'class_name' => $className,
            'signature' => $sig,
            'file_path' => $this->filePath,
            'start_line' => $node->getStartLine(),
            'end_line' => $node->getEndLine(),
            'code' => $this->extractCode($node->getStartLine(), $node->getEndLine()),
            'docstring' => $this->extractDocComment($node),
            'parameters' => $params,
            'return_type' => $returnType,
            'visibility' => $visibility,
            'modifiers' => [
                'is_static' => $node->isStatic(),
                'is_abstract' => $node->isAbstract(),
                'is_final' => $node->isFinal(),
            ],
        ];
    }

    private function processProperty(Stmt\Property $node, string $className): void
    {
        $visibility = $this->getVisibility($node);
        $type = $node->type ? $this->printType($node->type) : null;

        // PHP 8.4 property hooks — hooks live on Stmt\Property, not PropertyItem
        $hasHooks = property_exists($node, 'hooks') && !empty($node->hooks);

        // PHP 8.4 asymmetric visibility
        $setVisibility = null;
        if (method_exists($node, 'isPublicSet')) {
            if ($node->isPrivateSet()) {
                $setVisibility = 'private';
            } elseif ($node->isProtectedSet()) {
                $setVisibility = 'protected';
            }
        }

        foreach ($node->props as $prop) {
            $propName = $prop->name->toString();

            $sig = $visibility;
            if ($setVisibility) {
                $sig .= ' ' . $setVisibility . '(set)';
            }
            if ($node->isStatic()) {
                $sig .= ' static';
            }
            if ($node->isReadonly()) {
                $sig .= ' readonly';
            }
            if ($type) {
                $sig .= ' ' . $type;
            }
            $sig .= ' $' . $propName;

            $symbolData = [
                'name' => $propName,
                'type' => 'property',
                'namespace' => $this->currentNamespace,
                'class_name' => $className,
                'signature' => $sig,
                'file_path' => $this->filePath,
                'start_line' => $prop->getStartLine(),
                'end_line' => $prop->getEndLine(),
                'docstring' => $this->extractDocComment($node),
                'property_type' => $type,
                'visibility' => $visibility,
                'modifiers' => [
                    'is_static' => $node->isStatic(),
                    'is_readonly' => $node->isReadonly(),
                    'has_hooks' => $hasHooks,
                ],
            ];

            if ($setVisibility) {
                $symbolData['set_visibility'] = $setVisibility;
            }

            $this->symbols[] = $symbolData;
        }
    }

    private function processClassConstant(Stmt\ClassConst $node, string $className): void
    {
        $visibility = $this->getVisibility($node);
        // PHP 8.3 typed constants
        $type = $node->type ? $this->printType($node->type) : null;

        foreach ($node->consts as $const) {
            $name = $const->name->toString();
            $value = $this->extractConstExprValue($const->value);

            $sig = $visibility . ' const';
            if ($type) {
                $sig .= ' ' . $type;
            }
            $sig .= ' ' . $name;

            $this->symbols[] = [
                'name' => $name,
                'type' => 'constant',
                'namespace' => $this->currentNamespace,
                'class_name' => $className,
                'signature' => $sig,
                'file_path' => $this->filePath,
                'start_line' => $const->getStartLine(),
                'end_line' => $const->getEndLine(),
                'docstring' => $this->extractDocComment($node),
                'constant_type' => $type,
                'value' => $value,
                'visibility' => $visibility,
            ];
        }
    }

    // --- Helper methods ---

    private function getVisibility(Stmt\ClassMethod|Stmt\Property|Stmt\ClassConst $node): string
    {
        if ($node->isPrivate()) {
            return 'private';
        }
        if ($node->isProtected()) {
            return 'protected';
        }
        return 'public';
    }

    /**
     * @param Node\Param[] $params
     * @return array<int, array{name: string, type: ?string}>
     */
    private function extractParameters(array $params): array
    {
        $result = [];
        foreach ($params as $param) {
            $paramData = [
                'name' => ($param->var instanceof Expr\Variable && is_string($param->var->name))
                    ? $param->var->name
                    : '',
                'type' => $param->type ? $this->printType($param->type) : null,
            ];
            $result[] = $paramData;
        }
        return $result;
    }

    /**
     * @param array<int, array{name: string, type: ?string}> $params
     */
    private function buildParamString(array $params): string
    {
        $parts = [];
        foreach ($params as $param) {
            $s = '';
            if ($param['type']) {
                $s .= $param['type'] . ' ';
            }
            $s .= '$' . $param['name'];
            $parts[] = $s;
        }
        return implode(', ', $parts);
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
            $types = array_map(fn($t) => $this->printType($t), $type->types);
            return implode('|', $types);
        }
        if ($type instanceof Node\IntersectionType) {
            $types = array_map(fn($t) => $this->printType($t), $type->types);
            return implode('&', $types);
        }
        return $this->printer->prettyPrint([$type]);
    }

    private function resolveTypeName(Node\Name $name): string
    {
        $str = $name->toString();

        // Special keywords must never be namespace-qualified
        if (in_array(strtolower($str), ['self', 'static', 'parent'], true)) {
            return $str;
        }

        if ($name->isFullyQualified()) {
            return ltrim($str, '\\');
        }

        $first = $name->getFirst();
        if (isset($this->imports[$first])) {
            $rest = $name->slice(1);
            if ($rest !== null) {
                return $this->imports[$first] . '\\' . $rest->toString();
            }
            return $this->imports[$first];
        }

        // Relative to current namespace
        if ($this->currentNamespace) {
            return $this->currentNamespace . '\\' . $str;
        }

        return $str;
    }

    private function extractDocComment(Node $node): string
    {
        $doc = $node->getDocComment();
        if ($doc === null) {
            return '';
        }

        $text = $doc->getText();
        // Strip /** */ and leading * from each line
        $text = preg_replace('#^\s*/\*\*\s*#', '', $text);
        $text = preg_replace('#\s*\*/\s*$#', '', $text);
        $text = preg_replace('#^\s*\*\s?#m', '', $text);
        return trim($text);
    }

    private function extractCode(int $startLine, int $endLine, ?int $maxLines = null): string
    {
        if ($startLine < 1 || $endLine < $startLine) {
            return '';
        }

        $actualEnd = $endLine;
        if ($maxLines !== null && ($endLine - $startLine + 1) > $maxLines) {
            $actualEnd = $startLine + $maxLines - 1;
        }

        // 0-indexed
        $start = $startLine - 1;
        $end = min($actualEnd - 1, count($this->lines) - 1);

        if ($start >= count($this->lines)) {
            return '';
        }

        return implode("\n", array_slice($this->lines, $start, $end - $start + 1));
    }

    private function extractConstExprValue(Expr $expr): string
    {
        if ($expr instanceof Node\Scalar\String_) {
            return $expr->value;
        }
        if ($expr instanceof Node\Scalar\Int_) {
            return (string) $expr->value;
        }
        if ($expr instanceof Node\Scalar\Float_) {
            return (string) $expr->value;
        }
        if ($expr instanceof Expr\ConstFetch) {
            return $expr->name->toString();
        }
        if ($expr instanceof Expr\ClassConstFetch) {
            return $this->printType($expr->class) . '::' . $expr->name->toString();
        }
        // For complex expressions, use pretty printer
        try {
            return $this->printer->prettyPrintExpr($expr);
        } catch (\Throwable) {
            return '';
        }
    }

    private function buildClassSignature(
        string $name,
        ?string $extends,
        array $implements,
        array $modifiers
    ): string {
        $sig = '';
        if ($modifiers['is_abstract'] ?? false) {
            $sig .= 'abstract ';
        }
        if ($modifiers['is_final'] ?? false) {
            $sig .= 'final ';
        }
        if ($modifiers['is_readonly'] ?? false) {
            $sig .= 'readonly ';
        }
        $sig .= 'class ' . $name;
        if ($extends) {
            $sig .= ' extends ' . $extends;
        }
        if ($implements) {
            $sig .= ' implements ' . implode(', ', $implements);
        }
        return $sig;
    }
}
