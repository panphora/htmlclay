@echo off
setlocal

set "EXE=%~dp0htmlclay.exe"

:: Register the file type
reg add "HKCU\Software\Classes\.htmlclay" /ve /d "HTMLClay.Document" /f
reg add "HKCU\Software\Classes\HTMLClay.Document" /ve /d "HTML Clay File" /f
reg add "HKCU\Software\Classes\HTMLClay.Document\shell\open\command" /ve /d "\"%EXE%\" \"%%1\"" /f
:: -2 is a resource ID, not an index: a negative number in an icon location names
:: the resource itself, and resource 2 is the document icon (resource 1 is the app).
reg add "HKCU\Software\Classes\HTMLClay.Document\DefaultIcon" /ve /d "\"%EXE%\",-2" /f

:: Offer HTML Clay in the Open With list for regular HTML files without changing the default handler
reg add "HKCU\Software\Classes\.html\OpenWithProgids" /v "HTMLClay.Document" /t REG_NONE /f
reg add "HKCU\Software\Classes\.htm\OpenWithProgids" /v "HTMLClay.Document" /t REG_NONE /f

echo File association registered for .htmlclay
echo HTML Clay added to "Open with" for .html and .htm
echo Restart Explorer or log out/in for changes to take effect.
