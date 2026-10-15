package update

import "crypto/ed25519"

// TrustedKeys are the public halves of the keys that sign the update feed, by
// keyId. The release key's public half is added here when the key is generated,
// and its successor's beside it before a rotation, so installs never strand.
//
// While the map is empty the self-updater stays off and the tray keeps linking
// to the download page: a build that trusts no key could only ever refuse
// every feed.
var TrustedKeys = map[string]ed25519.PublicKey{
	"htmlclay-2026-10": {0xc5, 0x36, 0xe0, 0xb1, 0xba, 0xdd, 0x11, 0x20, 0xef, 0xc7, 0xf4, 0x5f, 0x1f, 0x0d, 0x55, 0x38, 0x54, 0x36, 0xeb, 0x65, 0x57, 0x78, 0x95, 0xb7, 0x5f, 0xaa, 0x32, 0x9b, 0x7b, 0x6e, 0x29, 0xb0},
}
