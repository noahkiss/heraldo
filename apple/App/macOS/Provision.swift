import Foundation
import HeraldoKit

/// Credentials from the shell, for an agent or a setup script: launched with
/// `--provision-stdin`, the app reads one JSON object from stdin,
///
///     {"url": "https://...", "read_token": "...", "write_token": "..."}
///
/// tests it against the server as Settings' "Save and test" does, and saves it
/// to the Keychain item, which iCloud Keychain carries to the other devices.
/// The tokens never reach argv, a file or the log. apple/scripts/provision.sh
/// feeds it through a FIFO with `open --stdin`.
enum Provision {
    static let flag = "--provision-stdin"
    /// The answer to a launch with the flag, one line in the log:
    /// "provision: saved ..." or "provision: failed: ...".
    static let logPrefix = "provision:"
    private static let maxInput = 64 * 1024

    static var requested: Bool { ProcessInfo.processInfo.arguments.contains(flag) }

    private struct Input: Decodable {
        var url: String
        var readToken: String
        var writeToken: String?

        enum CodingKeys: String, CodingKey {
            case url
            case readToken = "read_token"
            case writeToken = "write_token"
        }
    }

    /// Reads stdin, then saves. Logs the outcome without any value.
    @MainActor
    static func run(_ model: AppModel) async {
        let data = await Task.detached { FileHandle.standardInput.readData(upTo: maxInput) }.value
        do {
            guard !data.isEmpty else { throw ProvisionError("no input on stdin") }
            guard let input = try? JSONDecoder().decode(Input.self, from: data) else {
                throw ProvisionError("stdin is not a JSON object with url and read_token")
            }
            let health = try await model.save(Credentials(serverURL: input.url, readToken: input.readToken,
                                                          writeToken: input.writeToken))
            let write = (input.writeToken ?? "").isEmpty ? "no write token" : "write token set"
            log.notice("\(logPrefix, privacy: .public) saved in \(model.store.modeDescription, privacy: .public); heraldo \(health.version, privacy: .public), \(write, privacy: .public)")
        } catch {
            log.error("\(logPrefix, privacy: .public) failed: \(error.localizedDescription, privacy: .public)")
        }
    }

    private struct ProvisionError: LocalizedError {
        let errorDescription: String?
        init(_ message: String) { errorDescription = message }
    }
}

private extension FileHandle {
    /// Reads to end of file, or up to `limit` bytes.
    func readData(upTo limit: Int) -> Data {
        var out = Data()
        while out.count < limit, let chunk = try? read(upToCount: limit - out.count), !chunk.isEmpty {
            out.append(chunk)
        }
        return out
    }
}
