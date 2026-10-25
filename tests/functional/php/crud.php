<?php
// Synthetic CRUD through corral for the functional tests (no userland library:
// the PECL extension's low-level API only). The URI arrives as the FastCGI
// parameter HTTP_X_URI; nothing here is a real credential.
header('Content-Type: text/plain');
$uri = $_SERVER['HTTP_X_URI'] ?? '';
$id = $_SERVER['HTTP_X_ID'] ?? bin2hex(random_bytes(8));
try {
    $m = new MongoDB\Driver\Manager($uri);
    $bw = new MongoDB\Driver\BulkWrite();
    $bw->insert(['_id' => $id, 'v' => 1]);
    $m->executeBulkWrite('corraltest.php', $bw);
    $rows = $m->executeQuery('corraltest.php', new MongoDB\Driver\Query(['_id' => $id]))->toArray();
    echo count($rows) === 1 ? "OK\n" : "BAD missing\n";
} catch (Throwable $e) {
    echo 'ERR ' . get_class($e) . ' ' . $e->getCode() . "\n";
}
