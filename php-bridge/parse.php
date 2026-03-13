#!/usr/bin/env php
<?php
/**
 * PHP AST Bridge for rag-code-mcp
 *
 * Parses PHP files using nikic/php-parser and outputs JSON symbol data.
 *
 * Usage:
 *   php parse.php --file /path/to/file.php          # Single file
 *   echo "/path/to/file1.php\n/path/to/file2.php" | php parse.php --batch  # Batch via stdin
 *
 * Output: JSON array of symbols on stdout
 * Stderr: Parser warnings/errors
 */

declare(strict_types=1);

require_once __DIR__ . '/vendor/autoload.php';

use RagCode\Bridge\SymbolExtractor;
use RagCode\Bridge\SymfonyExtractor;
use PhpParser\ParserFactory;
use PhpParser\NodeTraverser;
use PhpParser\ErrorHandler\Collecting;

function main(): int
{
    $opts = getopt('', ['file:', 'batch', 'help']);

    if (isset($opts['help'])) {
        fwrite(STDERR, "Usage:\n");
        fwrite(STDERR, "  php parse.php --file /path/to/file.php\n");
        fwrite(STDERR, "  echo \"/path/file1.php\\n/path/file2.php\" | php parse.php --batch\n");
        return 0;
    }

    $files = [];

    if (isset($opts['file'])) {
        $file = $opts['file'];
        if (!is_file($file)) {
            fwrite(STDERR, "Error: file not found: {$file}\n");
            return 1;
        }
        $files[] = realpath($file);
    } elseif (isset($opts['batch']) || !posix_isatty(STDIN)) {
        $input = stream_get_contents(STDIN);
        if ($input === false || trim($input) === '') {
            fwrite(STDERR, "Error: no files provided on stdin\n");
            return 1;
        }
        foreach (explode("\n", trim($input)) as $line) {
            $line = trim($line);
            if ($line === '') continue;
            if (!is_file($line)) {
                fwrite(STDERR, "Warning: file not found, skipping: {$line}\n");
                continue;
            }
            $files[] = realpath($line);
        }
    } else {
        fwrite(STDERR, "Error: specify --file or --batch (pipe file list to stdin)\n");
        return 1;
    }

    if (empty($files)) {
        fwrite(STDERR, "Error: no valid files to process\n");
        return 1;
    }

    $parser = (new ParserFactory())->createForNewestSupportedVersion();
    $allSymbols = [];

    foreach ($files as $filePath) {
        try {
            $code = file_get_contents($filePath);
            if ($code === false) {
                fwrite(STDERR, "Warning: cannot read {$filePath}\n");
                continue;
            }

            $errorHandler = new Collecting();
            $stmts = $parser->parse($code, $errorHandler);

            if ($errorHandler->hasErrors()) {
                $maxErrors = 3;
                $errors = $errorHandler->getErrors();
                fwrite(STDERR, "Parser warnings in {$filePath}:\n");
                foreach (array_slice($errors, 0, $maxErrors) as $error) {
                    fwrite(STDERR, "  " . $error->getMessage() . "\n");
                }
                if (count($errors) > $maxErrors) {
                    fwrite(STDERR, "  ... and " . (count($errors) - $maxErrors) . " more\n");
                }
            }

            if ($stmts === null) {
                fwrite(STDERR, "Warning: failed to parse {$filePath}\n");
                continue;
            }

            $traverser = new NodeTraverser();
            $symbolExtractor = new SymbolExtractor($filePath, $code);
            $symfonyExtractor = new SymfonyExtractor($filePath, $code);
            $traverser->addVisitor($symbolExtractor);
            $traverser->addVisitor($symfonyExtractor);
            $traverser->traverse($stmts);

            $symbols = $symbolExtractor->getSymbols();

            // Enrich with Symfony metadata
            $symfonyMetadata = $symfonyExtractor->getMetadata();
            if (!empty($symfonyMetadata)) {
                foreach ($symbols as &$symbol) {
                    $key = ($symbol['namespace'] ?? '') . '\\' . $symbol['name'];
                    if (isset($symfonyMetadata[$key])) {
                        $symbol['metadata'] = array_merge(
                            $symbol['metadata'] ?? [],
                            $symfonyMetadata[$key]
                        );
                    }
                }
                unset($symbol);
            }

            $allSymbols = array_merge($allSymbols, $symbols);
        } catch (\Throwable $e) {
            fwrite(STDERR, "Error processing {$filePath}: {$e->getMessage()}\n");
        }
    }

    echo json_encode($allSymbols, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE);
    return 0;
}

exit(main());
