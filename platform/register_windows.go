//go:build windows

package platform

import (
	"errors"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Everything below lives under HKCU\Software\Classes, never HKLM. A per-user
// association needs no elevation, so registering is silent, and it is the only
// place a program that ships as a loose .exe has any business writing.
//
// These are the same four facts dist/windows/register.bat writes with reg.exe.
// That script asked the user to find it in the zip and run it, then restart
// Explorer; this is that script, run by the app itself on every launch.
const (
	progID      = "HTMLClay.Document"
	progIDLabel = "HTML Clay File"
	classesPath = `Software\Classes`
)

// docIconIndex selects the icon Explorer draws on a .htmlclay file. The value is
// NEGATIVE on purpose: in an icon location a positive number is an index into
// the executable's icon groups in whatever order the linker emitted them, and a
// negative number is the resource ID itself (documented on ExtractIconEx). The
// resource IDs are ours to choose, the link order is not, so the document icon
// is embedded as resource 2 by dist/windows/winres.json and named here by ID.
// Resource 1 is the app icon, which is what Explorer draws on the .exe.
const docIconIndex = ",-2"

var (
	modshell32         = windows.NewLazySystemDLL("shell32.dll")
	procSHChangeNotify = modshell32.NewProc("SHChangeNotify")
	modadvapi32        = windows.NewLazySystemDLL("advapi32.dll")
	procRegSetValueEx  = modadvapi32.NewProc("RegSetValueExW")
)

const (
	shcneAssocChanged = 0x08000000
	shcnfIDList       = 0x0000
)

func registerFileTypes(exePath string) error {
	quoted := `"` + exePath + `"`
	defaults := []struct{ subkey, value string }{
		{`.htmlclay`, progID},
		{progID, progIDLabel},
		{progID + `\shell\open\command`, quoted + ` "%1"`},
		{progID + `\DefaultIcon`, quoted + docIconIndex},
	}

	// Every write is attempted even after one fails, and the shell is told about
	// whatever did land. Returning at the first error instead would mean a policy
	// that allows the core association but refuses the optional Open With entry
	// leaves Explorer serving its cached association forever: the notify below is
	// skipped, the next launch takes the same path, and the registration that did
	// succeed stays invisible until the user signs out.
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	wrote := false
	for _, d := range defaults {
		changed, err := setClassDefault(d.subkey, d.value)
		keep(err)
		wrote = wrote || changed
	}

	// Offering HTML Clay for .html and .htm must not take those extensions over.
	// Creating HKCU\Software\Classes\.html with no default value leaves the
	// merged HKEY_CLASSES_ROOT view reading the ProgID out of HKLM, so the user's
	// browser keeps them; only the Open With list gains an entry.
	for _, ext := range []string{`.html`, `.htm`} {
		changed, err := addOpenWithProgID(ext)
		keep(err)
		wrote = wrote || changed
	}

	if wrote {
		// Without this the shell serves the association it cached at logon, so a
		// first run would register correctly and the file would still open with
		// whatever owned it before, until the user signed out.
		notifyAssociationsChanged()
	}
	return firstErr
}

func unregisterFileTypes() error {
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// The extension key is removed only while it is still pointing at us. If
	// another program has taken .htmlclay over since we registered, that key holds
	// their ProgID and their subkeys, and deleting the tree would uninstall their
	// association along with ours. The ProgID tree below is ours either way.
	if classDefaultIs(`.htmlclay`, progID) {
		keep(deleteClassTree(`.htmlclay`))
	}
	keep(deleteClassTree(progID))

	// Only our own value comes out of OpenWithProgids. The key is shared: every
	// other program that offers itself for .html has a value there too, and
	// deleting the key would take the whole Open With list with it.
	for _, ext := range []string{`.html`, `.htm`} {
		keep(deleteOpenWithProgID(ext))
	}

	notifyAssociationsChanged()
	return firstErr
}

// setClassDefault writes value as the default value of a key under
// HKCU\Software\Classes, and reports whether it had to. The read first is what
// makes the common launch cost four registry reads and no writes at all, and it
// is also what keeps notifyAssociationsChanged (which makes every shell process
// rebuild its association cache) off the path of an ordinary start.
func setClassDefault(subkey, value string) (bool, error) {
	path := classesPath + `\` + subkey
	if k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.QUERY_VALUE); err == nil {
		current, _, readErr := k.GetStringValue("")
		k.Close()
		if readErr == nil && current == value {
			return false, nil
		}
	}

	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		return false, err
	}
	defer k.Close()
	if err := k.SetStringValue("", value); err != nil {
		return false, err
	}
	return true, nil
}

// classDefaultIs reports whether the default value of a key under
// HKCU\Software\Classes is exactly value. A key that is missing, unreadable, or
// holding something else reads as not ours, which keeps unregistering off
// anything this program did not write.
func classDefaultIs(subkey, value string) bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, classesPath+`\`+subkey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	current, _, err := k.GetStringValue("")
	return err == nil && current == value
}

// setNoneValue writes a name with no data and no type, which is what an
// OpenWithProgids member is: the shell reads the value names under that key and
// never their contents. x/sys's registry package can write every type except
// REG_NONE and its setValue is unexported, so this goes to RegSetValueExW
// directly rather than storing an empty string under some other type, which
// keeps the value byte for byte what dist/windows/register.bat has always written.
func setNoneValue(k registry.Key, name string) error {
	if err := procRegSetValueEx.Find(); err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	status, _, _ := procRegSetValueEx.Call(uintptr(k), uintptr(unsafe.Pointer(p)), 0, uintptr(registry.NONE), 0, 0)
	// The unsafe.Pointer-to-uintptr rule only keeps a pointer alive when the
	// conversion sits in the argument list of syscall.Syscall itself. LazyProc.Call
	// takes a ...uintptr, so the name has to be held here by hand or the collector
	// is free to reclaim the string while the call is still reading it.
	runtime.KeepAlive(p)
	if status != 0 {
		return syscall.Errno(status)
	}
	return nil
}

// addOpenWithProgID lists HTML Clay as an alternative handler for ext.
//
// The shell builds the Open With list from the value NAMES under
// OpenWithProgids and never reads their data, but the value is still written as
// a real REG_NONE so it matches what register.bat writes exactly.
func addOpenWithProgID(ext string) (bool, error) {
	path := classesPath + `\` + ext + `\OpenWithProgids`
	if k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.QUERY_VALUE); err == nil {
		_, _, readErr := k.GetValue(progID, nil)
		k.Close()
		if readErr == nil {
			return false, nil
		}
	}

	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		return false, err
	}
	defer k.Close()
	if err := setNoneValue(k, progID); err != nil {
		return false, err
	}
	return true, nil
}

func deleteOpenWithProgID(ext string) error {
	path := classesPath + `\` + ext + `\OpenWithProgids`
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		return ignoreMissing(err)
	}
	defer k.Close()
	return ignoreMissing(k.DeleteValue(progID))
}

// deleteClassTree removes a key under HKCU\Software\Classes and everything
// below it. RegDeleteKey refuses a key that still has subkeys, and these keys do
// have them: shell\open\command is three deep, and Explorer adds its own
// children to an extension key over time.
func deleteClassTree(subkey string) error {
	return deleteTree(registry.CURRENT_USER, classesPath+`\`+subkey)
}

func deleteTree(root registry.Key, path string) error {
	k, err := registry.OpenKey(root, path, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return ignoreMissing(err)
	}
	names, err := k.ReadSubKeyNames(-1)
	k.Close()
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := deleteTree(root, path+`\`+name); err != nil {
			return err
		}
	}
	return ignoreMissing(registry.DeleteKey(root, path))
}

// ignoreMissing turns "it was not there" into success, so unregistering twice,
// or unregistering a machine that was never registered, is not an error.
func ignoreMissing(err error) error {
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	return err
}

// notifyAssociationsChanged tells the shell to drop its association cache.
// SHChangeNotify returns nothing and cannot fail in a way worth acting on: the
// worst case is that Explorer picks the change up at the next sign-in instead.
func notifyAssociationsChanged() {
	procSHChangeNotify.Call(uintptr(shcneAssocChanged), uintptr(shcnfIDList), 0, 0)
}
