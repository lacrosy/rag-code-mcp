<?php
/**
 * PHP Bridge HTTP Server for ragcode.
 *
 * Wraps parse.php as an HTTP microservice so developers don't need PHP installed locally.
 * Runs inside Docker with the project directory mounted at /workspace.
 *
 * Endpoints:
 *   POST /parse       — parse files, body: {"files": ["/workspace/src/Foo.php", ...]}
 *   GET  /health      — health check
 *
 * Usage:
 *   php -S 0.0.0.0:9100 server.php
 */

declare(strict_types=1);

// Bootstrap: load the same autoloader and classes as parse.php
require_once __DIR__ . '/vendor/autoload.php';

use PhpParser\ParserFactory;
use PhpParser\NodeTraverser;
use RagCode\Bridge\SymbolExtractor;
use RagCode\Bridge\SymfonyExtractor;
use RagCode\Bridge\ExtractorLoader;

// ─── Routing ─────────────────────────────────────────────────────────────────

$method = $_SERVER['REQUEST_METHOD'] ?? 'GET';
$uri = parse_url($_SERVER['REQUEST_URI'] ?? '/', PHP_URL_PATH);

if ($uri === '/health' && $method === 'GET') {
    header('Content-Type: application/json');
    echo json_encode([
        'status' => 'ok',
        'parser' => 'nikic/php-parser',
        'php_version' => PHP_VERSION,
    ]);
    return true;
}

if ($uri === '/parse' && $method === 'POST') {
    handleParse();
    return true;
}

// 404
http_response_code(404);
header('Content-Type: application/json');
echo json_encode(['error' => 'Not found. Use POST /parse or GET /health']);
return true;

// ─── Parse Handler ───────────────────────────────────────────────────────────

function handleParse(): void
{
    $body = file_get_contents('php://input');
    $request = json_decode($body, true);

    if (!$request || !isset($request['files']) || !is_array($request['files'])) {
        http_response_code(400);
        header('Content-Type: application/json');
        echo json_encode(['error' => 'Expected JSON body: {"files": ["/path/to/file.php", ...]}']);
        return;
    }

    $files = $request['files'];
    $extractorsDir = $request['extractors_dir'] ?? null;

    // Discover custom extractors if directory provided
    $extractorClasses = [];
    if ($extractorsDir && is_string($extractorsDir) && is_dir($extractorsDir)) {
        $extractorClasses = ExtractorLoader::loadFromDirectory($extractorsDir);
    }

    $parser = (new ParserFactory())->createForNewestSupportedVersion();
    $allSymbols = [];
    $errors = [];

    foreach ($files as $filePath) {
        if (!is_string($filePath) || !file_exists($filePath)) {
            $errors[] = "File not found: $filePath";
            continue;
        }

        $code = file_get_contents($filePath);
        if ($code === false) {
            $errors[] = "Cannot read: $filePath";
            continue;
        }

        try {
            $stmts = $parser->parse($code);
            if ($stmts === null) {
                $errors[] = "Parse returned null: $filePath";
                continue;
            }

            // Symbol extraction
            $extractor = new SymbolExtractor($filePath, $code);
            $symfonyExtractor = new SymfonyExtractor($filePath, $code);

            $traverser = new NodeTraverser();
            $traverser->addVisitor($extractor);
            $traverser->addVisitor($symfonyExtractor);

            // Add custom extractors
            $customExtractors = ExtractorLoader::instantiate($extractorClasses, $filePath, $code);
            foreach ($customExtractors as $ext) {
                $traverser->addVisitor($ext);
            }

            $traverser->traverse($stmts);

            $symbols = $extractor->getSymbols();

            // Collect all metadata: Symfony + custom extractors
            $allMeta = [];
            foreach ($symfonyExtractor->getMetadata() as $fqn => $meta) {
                $allMeta[$fqn] = array_merge($allMeta[$fqn] ?? [], $meta);
            }
            foreach ($customExtractors as $ext) {
                foreach ($ext->getMetadata() as $fqn => $meta) {
                    $allMeta[$fqn] = array_merge($allMeta[$fqn] ?? [], $meta);
                }
            }

            // Merge metadata into symbols
            foreach ($symbols as &$symbol) {
                $fqn = ($symbol['namespace'] ? $symbol['namespace'] . '\\' : '') . $symbol['name'];
                if (isset($allMeta[$fqn])) {
                    $symbol['metadata'] = array_merge($symbol['metadata'] ?? [], $allMeta[$fqn]);
                }
            }
            unset($symbol);

            $allSymbols = array_merge($allSymbols, $symbols);
        } catch (\Throwable $e) {
            $errors[] = "Error parsing $filePath: " . $e->getMessage();
        }
    }

    header('Content-Type: application/json');
    $response = ['symbols' => $allSymbols];
    if ($errors) {
        $response['errors'] = $errors;
    }
    echo json_encode($response, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE);
}
