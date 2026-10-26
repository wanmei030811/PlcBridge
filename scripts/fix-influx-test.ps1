$f = 'F:\Project\PlcBridge\internal\infra\target\influxdb\influxdb_test.go'
$c = [System.IO.File]::ReadAllText($f)
$c = $c -replace '(?<![\w.])PointValue\{', 'target.PointValue{'
$c = $c -replace 'pv       PointValue', 'pv       target.PointValue'
$c = $c -replace 'batch := \[\]PointValue', 'batch := []target.PointValue'
$c = $c -replace '\[\]PointValue\{\{', '[]target.PointValue{{'
[System.IO.File]::WriteAllText($f, $c, (New-Object System.Text.UTF8Encoding($false)))
Write-Output "ok"
