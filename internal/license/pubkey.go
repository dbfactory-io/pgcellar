package license

import "crypto/ed25519"

// PublicKeyBase64 is the key licences are checked against (base64, 32 bytes).
// The matching private key never enters this repository.
const PublicKeyBase64 = "SpCSjN3laWziMfBysVNb/sc9GJclGcndih9got2AVNk="

// PublicKey is the embedded verification key.
func PublicKey() ed25519.PublicKey {
	k, err := DecodePublic(PublicKeyBase64)
	if err != nil {
		return nil
	}
	return k
}
