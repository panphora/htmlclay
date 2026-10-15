//go:build !darwin

package sparkle

import "errors"

var errNotMac = errors.New("Sparkle runs on macOS only")

func Load(string) error { return errNotMac }

func Start(func(Event)) {}

func SetSkip(func(string) string) {}

func InstallNow() {}

func InfoString(string) string { return "" }

func BundleWritable(string) error { return errNotMac }
