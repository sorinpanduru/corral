<?php
// A single ping through the URI in HTTP_X_URI; prints OK or the exception class.
header('Content-Type: text/plain');
try {
    $m = new MongoDB\Driver\Manager($_SERVER['HTTP_X_URI'] ?? '');
    $m->executeCommand('admin', new MongoDB\Driver\Command(['ping' => 1]));
    echo "OK\n";
} catch (Throwable $e) {
    echo 'ERR ' . get_class($e) . ' ' . $e->getCode() . "\n";
}
