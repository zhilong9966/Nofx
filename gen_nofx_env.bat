@echo off
setlocal EnableExtensions

REM Usage:
REM   gen_nofx_env.bat [OUTDIR] [BACKEND_PORT] [FRONTEND_PORT] [TZ]
REM Example:
REM   gen_nofx_env.bat E:\nofx-dev\nofx-dev 8080 3000 Asia/Tokyo

set "OUTDIR=%~1"
if not defined OUTDIR set "OUTDIR=%CD%"

set "BACKEND_PORT=%~2"
if not defined BACKEND_PORT set "BACKEND_PORT=8080"

set "FRONTEND_PORT=%~3"
if not defined FRONTEND_PORT set "FRONTEND_PORT=3000"

set "TZ=%~4"
if not defined TZ set "TZ=Asia/Tokyo"

set "PS1=%TEMP%\nofx_gen_env_%RANDOM%%RANDOM%.ps1"

> "%PS1%"  echo $ErrorActionPreference = 'Stop'
>>"%PS1%" echo $outDir = $env:OUTDIR
>>"%PS1%" echo if ([string]::IsNullOrWhiteSpace($outDir)) { $outDir = (Get-Location).Path }
>>"%PS1%" echo New-Item -ItemType Directory -Force -Path $outDir ^| Out-Null
>>"%PS1%" echo
>>"%PS1%" echo function New-RandB64([int]$n=32){
>>"%PS1%" echo   $bytes = New-Object byte[] $n
>>"%PS1%" echo   [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
>>"%PS1%" echo   [Convert]::ToBase64String($bytes)
>>"%PS1%" echo }
>>"%PS1%" echo
>>"%PS1%" echo function Asn1-Len([int]$len){
>>"%PS1%" echo   if ($len -lt 128) { return ,([byte]$len) }
>>"%PS1%" echo   $tmp = New-Object System.Collections.Generic.List[byte]
>>"%PS1%" echo   $v = $len
>>"%PS1%" echo   while ($v -gt 0) { $tmp.Insert(0, [byte]($v -band 0xFF)); $v = $v -shr 8 }
>>"%PS1%" echo   $out = New-Object byte[] (1 + $tmp.Count)
>>"%PS1%" echo   $out[0] = [byte](0x80 + $tmp.Count)
>>"%PS1%" echo   [Array]::Copy($tmp.ToArray(), 0, $out, 1, $tmp.Count)
>>"%PS1%" echo   return ,$out
>>"%PS1%" echo }
>>"%PS1%" echo
>>"%PS1%" echo function Asn1-Int([byte[]]$be){
>>"%PS1%" echo   if (-not $be) { $be =  }
>>"%PS1%" echo   # trim leading zeros
>>"%PS1%" echo   $i = 0
>>"%PS1%" echo   while ($i -lt ($be.Length-1) -and $be[$i] -eq 0) { $i++ }
>>"%PS1%" echo   if ($i -gt 0) { $be = $be[$i..($be.Length-1)] }
>>"%PS1%" echo   # if highest bit set, prefix 0x00 for positive integer
>>"%PS1%" echo   if (($be[0] -band 0x80) -ne 0) { $be = ,([byte]0) + $be }
>>"%PS1%" echo   $len = Asn1-Len $be.Length
>>"%PS1%" echo   return ,([byte]0x02) + $len + $be
>>"%PS1%" echo }
>>"%PS1%" echo
>>"%PS1%" echo function Asn1-Seq([byte[]]$content){
>>"%PS1%" echo   $len = Asn1-Len $content.Length
>>"%PS1%" echo   return ,([byte]0x30) + $len + $content
>>"%PS1%" echo }
>>"%PS1%" echo
>>"%PS1%" echo function Export-RsaPkcs1Pem([int]$bits=2048){
>>"%PS1%" echo   $rsa = New-Object System.Security.Cryptography.RSACryptoServiceProvider($bits)
>>"%PS1%" echo   $p = $rsa.ExportParameters($true)
>>"%PS1%" echo   $ver = [byte[]](0x02,0x01,0x00) # INTEGER 0
>>"%PS1%" echo   $content = New-Object System.Collections.Generic.List[byte]
>>"%PS1%" echo   $content.AddRange($ver)
>>"%PS1%" echo   $content.AddRange((Asn1-Int $p.Modulus))
>>"%PS1%" echo   $content.AddRange((Asn1-Int $p.Exponent))
>>"%PS1%" echo   $content.AddRange((Asn1-Int $p.D))
>>"%PS1%" echo   $content.AddRange((Asn1-Int $p.P))
>>"%PS1%" echo   $content.AddRange((Asn1-Int $p.Q))
>>"%PS1%" echo   $content.AddRange((Asn1-Int $p.DP))
>>"%PS1%" echo   $content.AddRange((Asn1-Int $p.DQ))
>>"%PS1%" echo   $content.AddRange((Asn1-Int $p.InverseQ))
>>"%PS1%" echo   $der = Asn1-Seq ($content.ToArray())
>>"%PS1%" echo   $b64 = [Convert]::ToBase64String($der, [System.Base64FormattingOptions]::InsertLineBreaks)
>>"%PS1%" echo   $pem = "-----BEGIN RSA PRIVATE KEY-----`n" + $b64 + "`n-----END RSA PRIVATE KEY-----"
>>"%PS1%" echo   return $pem
>>"%PS1%" echo }
>>"%PS1%" echo
>>"%PS1%" echo $backend  = [int]$env:BACKEND_PORT
>>"%PS1%" echo $frontend = [int]$env:FRONTEND_PORT
>>"%PS1%" echo $tz = $env:TZ
>>"%PS1%" echo if ([string]::IsNullOrWhiteSpace($tz)) { $tz = 'Asia/Tokyo' }
>>"%PS1%" echo
>>"%PS1%" echo $jwt     = New-RandB64 32
>>"%PS1%" echo $dataKey = New-RandB64 32
>>"%PS1%" echo $pem = Export-RsaPkcs1Pem 2048
>>"%PS1%" echo $rsaEscaped = ($pem -replace "`r?`n","\\n")
>>"%PS1%" echo
>>"%PS1%" echo $envPath = Join-Path $outDir ".env"
>>"%PS1%" echo $content = @"
>>"%PS1%" echo # NOFX Configuration
>>"%PS1%" echo NOFX_BACKEND_PORT=$backend
>>"%PS1%" echo NOFX_FRONTEND_PORT=$frontend
>>"%PS1%" echo TZ=$tz
>>"%PS1%" echo JWT_SECRET=$jwt
>>"%PS1%" echo DATA_ENCRYPTION_KEY=$dataKey
>>"%PS1%" echo RSA_PRIVATE_KEY=$rsaEscaped
>>"%PS1%" echo "@
>>"%PS1%" echo
>>"%PS1%" echo Set-Content -Path $envPath -Encoding ASCII -Value $content
>>"%PS1%" echo Write-Host ("OK: .env generated -> " + $envPath)

set "OUTDIR=%OUTDIR%"
set "BACKEND_PORT=%BACKEND_PORT%"
set "FRONTEND_PORT=%FRONTEND_PORT%"
set "TZ=%TZ%"

powershell -NoProfile -ExecutionPolicy Bypass -File "%PS1%"
set "RC=%ERRORLEVEL%"

del "%PS1%" >nul 2>&1
exit /b %RC%
