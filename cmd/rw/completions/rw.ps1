# PowerShell completion for rw (Relayweft), for Windows PowerShell 5.1 and
# PowerShell 7. Add this line to your profile (notepad $PROFILE):
#   rw completion powershell | Out-String | Invoke-Expression

Register-ArgumentCompleter -Native -CommandName 'rw', 'rw.exe' -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)

    # The words after rw up to the cursor; the last one is being completed.
    $words = New-Object System.Collections.Generic.List[string]
    $elements = $commandAst.CommandElements
    for ($i = 1; $i -lt $elements.Count; $i++) {
        $e = $elements[$i]
        if ($e.Extent.StartOffset -ge $cursorPosition) { break }
        $text = $e.Extent.Text
        if ($e.Extent.EndOffset -gt $cursorPosition) {
            $text = $text.Substring(0, $cursorPosition - $e.Extent.StartOffset)
        } elseif ($e -is [System.Management.Automation.Language.StringConstantExpressionAst]) {
            $text = $e.Value # without its quotes
        }
        $words.Add($text)
    }
    if ($wordToComplete -eq '') { $words.Add('') }

    # Windows PowerShell 5.1 drops empty arguments to programs and mangles
    # quotes in them, so the words go through the environment.
    $sep = [string][char]31
    $env:RW_COMPLETE_WORDS = ($words -join $sep) + $sep
    $encoding = $null
    try {
        $encoding = [Console]::OutputEncoding
        [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding $false
    } catch { }
    try {
        $out = @(& rw __complete 2>$null)
    } catch {
        $out = @()
    } finally {
        Remove-Item Env:\RW_COMPLETE_WORDS -ErrorAction SilentlyContinue
        if ($null -ne $encoding) {
            try { [Console]::OutputEncoding = $encoding } catch { }
        }
    }

    # files and dirs: return nothing, so PowerShell completes paths itself.
    if ($out.Count -lt 2 -or $out[0] -eq 'files' -or $out[0] -eq 'dirs') { return }
    for ($i = 1; $i -lt $out.Count; $i++) {
        $parts = $out[$i] -split "`t", 2
        $value = $parts[0]
        if ($value -eq '') { continue }
        $tip = $value
        if ($parts.Count -gt 1 -and $parts[1] -ne '') { $tip = $parts[1] }
        $text = $value
        if ($text -match '[\s''"`$;,(){}|&<>@#]') { $text = "'" + ($text -replace "'", "''") + "'" }
        $type = 'ParameterValue'
        if ($value.StartsWith('-')) { $type = 'ParameterName' }
        New-Object System.Management.Automation.CompletionResult $text, $value, $type, $tip
    }
}
