package update

import "crypto/ed25519"

// TrustedKeys are the public halves of the keys that sign the update feed, by
// keyId. The release key's public half is added here when the key is generated,
// and its successor's beside it before a rotation, so installs never strand.
//
// While the map is empty the self-updater stays off and the tray keeps linking
// to the download page: a build that trusts no key could only ever refuse
// every feed.
var TrustedKeys = map[string]ed25519.PublicKey{}
