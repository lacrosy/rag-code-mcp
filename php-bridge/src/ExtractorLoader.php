<?php

declare(strict_types=1);

namespace RagCode\Bridge;

/**
 * Discovers and loads custom extractor plugins from a directory.
 *
 * Looks for *Extractor.php files that contain classes extending AbstractExtractor.
 * Used by both parse.php (CLI) and server.php (HTTP) to load project-specific extractors.
 */
class ExtractorLoader
{
    /**
     * Discover extractor class names from a directory.
     *
     * Scans for *Extractor.php files, requires them, and returns class names
     * that extend AbstractExtractor.
     *
     * @param string $dir Absolute path to directory containing extractor files
     * @return string[] Fully qualified class names
     */
    public static function loadFromDirectory(string $dir): array
    {
        if (!is_dir($dir)) {
            return [];
        }

        $classes = [];
        $files = glob($dir . '/*Extractor.php');
        if ($files === false) {
            return [];
        }

        // Get classes before loading to diff against
        $before = get_declared_classes();

        foreach ($files as $file) {
            require_once $file;
        }

        $after = get_declared_classes();
        $newClasses = array_diff($after, $before);

        foreach ($newClasses as $className) {
            if (is_subclass_of($className, AbstractExtractor::class)) {
                $classes[] = $className;
            }
        }

        return $classes;
    }

    /**
     * Instantiate extractor classes for a given file.
     *
     * @param string[] $classNames Fully qualified class names
     * @param string $filePath Path to the PHP file being parsed
     * @param string $code Source code of the file
     * @return AbstractExtractor[]
     */
    public static function instantiate(array $classNames, string $filePath, string $code): array
    {
        $instances = [];
        foreach ($classNames as $className) {
            try {
                $instances[] = new $className($filePath, $code);
            } catch (\Throwable $e) {
                fwrite(STDERR, "Warning: failed to instantiate extractor {$className}: {$e->getMessage()}\n");
            }
        }
        return $instances;
    }
}
