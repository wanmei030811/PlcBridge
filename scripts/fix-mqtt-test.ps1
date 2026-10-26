$f = 'F:\Project\PlcBridge\internal\infra\target\mqtt\mqtt_test.go'
$c = [System.IO.File]::ReadAllText($f)
$c = $c -replace 'package target', 'package mqtt'
$c = $c -replace 'newMQTTPlugin\(\)', 'newPlugin()'
$c = $c -replace '(?<![\w.])PointValue\{', 'target.PointValue{'
$c = $c -replace 'cli TargetClient', 'cli target.TargetClient'
$c = $c -replace '"github.com/plcbridge/plcbridge/internal/platform"', '"github.com/plcbridge/plcbridge/internal/infra/target"`r`n`t"github.com/plcbridge/plcbridge/internal/platform"'
[System.IO.File]::WriteAllText($f, $c, (New-Object System.Text.UTF8Encoding($false)))
Write-Output "ok"
