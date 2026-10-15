import CryptoKit
import Foundation

let env = ProcessInfo.processInfo.environment
guard let b64 = env["SPARKLE_ED_PRIVATE_KEY"], let seed = Data(base64Encoded: b64.trimmingCharacters(in: .whitespacesAndNewlines)), seed.count == 32,
      let key = try? Curve25519.Signing.PrivateKey(rawRepresentation: seed) else {
    FileHandle.standardError.write("SPARKLE_ED_PRIVATE_KEY is not a base64 32-byte seed\n".data(using: .utf8)!)
    exit(1)
}
print(key.publicKey.rawRepresentation.base64EncodedString())
