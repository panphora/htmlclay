@echo off
setlocal

set "EXE=%~dp0htmlclay.exe"

:: Register the file type
reg add "HKCU\Software\Classes\.htmlclay" /ve /d "HTMLClay.Document" /f
reg add "HKCU\Software\Classes\HTMLClay.Document" /ve /d "HTML Clay File" /f
reg add "HKCU\Software\Classes\HTMLClay.Document\shell\open\command" /ve /d "\"%EXE%\" \"%%1\"" /f

echo File association registered for .htmlclay
echo Restart Explorer or log out/in for changes to take effect.
