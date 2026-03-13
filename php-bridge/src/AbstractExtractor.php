<?php

declare(strict_types=1);

namespace RagCode\Bridge;

use PhpParser\Node;
use PhpParser\NodeVisitorAbstract;
use PhpParser\Node\Stmt;
use PhpParser\Node\Attribute;

/**
 * Base class for custom metadata extractors (plugins).
 *
 * Subclass this to create project-specific extractors that enrich PHP symbols
 * with domain metadata. The extractor is used as a nikic/php-parser NodeVisitor.
 *
 * Automatically tracks:
 * - Current namespace
 * - Use/import statements
 *
 * Provides shared helpers for resolving type names, extracting attribute args, etc.
 *
 * Usage:
 *   class MyExtractor extends AbstractExtractor {
 *       public function enterNode(Node $node): ?int {
 *           parent::enterNode($node); // MUST call parent for namespace/import tracking
 *           // ... your logic ...
 *           return null;
 *       }
 *   }
 */
abstract class AbstractExtractor extends NodeVisitorAbstract
{
    protected string $filePath;
    protected string $code;
    protected string $currentNamespace = '';
    /** @var array<string, string> alias => FQN */
    protected array $imports = [];

    /** @var array<string, array<string, mixed>> keyed by FQN */
    protected array $metadata = [];

    public function __construct(string $filePath, string $code)
    {
        $this->filePath = $filePath;
        $this->code = $code;
    }

    /**
     * Returns collected metadata keyed by class FQN.
     * Values are merged into the symbol's metadata field.
     *
     * @return array<string, array<string, mixed>>
     */
    public function getMetadata(): array
    {
        return $this->metadata;
    }

    /**
     * Tracks namespace and use statements automatically.
     * Subclasses MUST call parent::enterNode($node) to keep tracking working.
     */
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

        return null;
    }

    // --- Shared helpers ---

    /**
     * Build the FQN for a class node in the current namespace.
     */
    protected function classFQN(Stmt\Class_|Stmt\Enum_ $node): string
    {
        $name = $node->name->toString();
        return $this->currentNamespace ? $this->currentNamespace . '\\' . $name : $name;
    }

    /**
     * Resolve a Name node to its fully qualified form using current imports.
     */
    protected function resolveTypeName(Node\Name $name): string
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

    /**
     * Resolve an Attribute's name to FQN.
     */
    protected function resolveAttributeName(Attribute $attr): string
    {
        return $this->resolveTypeName($attr->name);
    }

    /**
     * Get the short (unqualified) class name from a FQN.
     */
    protected function shortClassName(string $fqn): string
    {
        $parts = explode('\\', $fqn);
        return end($parts);
    }

    /**
     * Extract named and positional arguments from an Attribute node.
     *
     * @return array<string|int, mixed>
     */
    protected function extractAttributeArgs(Attribute $attr): array
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

    /**
     * Extract a scalar value from an expression node.
     */
    protected function extractArgValue(Node\Expr $expr): mixed
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
            $class = $this->printType($expr->class);
            $const = $expr->name->toString();
            if ($const === 'class') {
                return $class;
            }
            return $class . '::' . $const;
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

    /**
     * Print a type node as a string, resolving names.
     */
    protected function printType(Node $type): string
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

    /**
     * Store metadata for a class FQN, merging with any existing metadata.
     */
    protected function addMetadata(string $fqn, array $meta): void
    {
        if (empty($meta)) {
            return;
        }
        $this->metadata[$fqn] = array_merge($this->metadata[$fqn] ?? [], $meta);
    }
}
