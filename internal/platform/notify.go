package platform

// Notify shows a best-effort native error message to the user. It exists for
// failures the user must see even though no window opened, such as opening a
// file outside the home directory. The error is returned so callers can fall
// back to logging when no native mechanism is available.
func Notify(title, message string) error {
	return notify(title, message)
}

// Inform shows a best-effort native notice that something worked, for the rare
// case where nothing else on screen confirms it. Unlike Notify it is not urgent:
// it fades on its own where the platform allows, and carries no error icon.
func Inform(title, message string) error {
	return inform(title, message)
}
