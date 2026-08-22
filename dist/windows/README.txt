HTML Clay for Windows
=====================

Unzip both files somewhere you will keep them, such as
%LOCALAPPDATA%\Programs\HTMLClay, and run htmlclay.exe once.

That first run registers .htmlclay files with Windows for your account only, so
no administrator prompt appears. From then on, double-clicking a .htmlclay file
opens it. HTML Clay adds itself to the "Open with" list for .html and .htm too,
without taking those file types away from your browser.

Every later launch checks the registration again, so moving htmlclay.exe to
another folder is enough to fix it: just run it once from its new home.


Removing HTML Clay
------------------

Quit HTML Clay from the tray icon, then run:

    htmlclay.exe --unregister

That removes the file associations and the Start on Login entry. Delete
htmlclay.exe afterwards; starting it again puts both back.

Your files are never touched. Your settings and the saved version history of
every file you have edited stay in %APPDATA%\htmlclay, so delete that folder too
if you want nothing left.


register.bat
------------

The fallback for a machine where the app's own registration is refused, for
example by a policy that blocks writes to HKCU\Software\Classes. You should not
need it. It writes the same associations with reg.exe, and it will be dropped
from a future release.
