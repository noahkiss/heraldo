import Foundation
import HeraldoKit
import UserNotifications

/// Credentials from the shell, for an agent or a setup script: launched with
/// `--provision-stdin`, the app reads one JSON object from stdin,
///
///     {"url": "https://...", "read_token": "...", "write_token": "..."}
///
/// tests it against the server and saves it to the Keychain item, which
/// iCloud Keychain carries to the other devices. apple/scripts/provision.sh
/// feeds it through a FIFO with `open --stdin`.
///
/// The rules (README, "Provision from the shell"):
/// 1. Write-only. Nothing here reads credentials back out, and the log names
///    the outcome and the server host, never a token.
/// 2. Launch-time only, through this flag and stdin. No URL scheme, intent or
///    other entry point reaches it.
/// 3. The read token must pass the server check and the write token the
///    scope check (`HeraldoClient.checkWriteToken`, which stores nothing).
///    On a failure nothing is saved.
/// 4. Settings for a different server are replaced only with `--replace`.
/// 5. A save posts a local notification, so a change the person did not
///    make is visible.
/// 6. stdin must be a pipe or FIFO, never a terminal.
enum Provision {
    static let flag = "--provision-stdin"
    static let replaceFlag = "--replace"
    private static let maxInput = 64 * 1024

    static var requested: Bool { ProcessInfo.processInfo.arguments.contains(flag) }
    private static var replace: Bool { ProcessInfo.processInfo.arguments.contains(replaceFlag) }

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

    /// Reads stdin, checks, then saves. Logs one "provision:" line: "saved"
    /// or "failed", with the server host and no value.
    @MainActor
    static func run(_ model: AppModel) async {
        var host = "unknown host"
        do {
            guard stdinIsPipe() else { throw ProvisionError("stdin is not a pipe or FIFO") }
            let data = await Task.detached { FileHandle.standardInput.readData(upTo: maxInput) }.value
            guard !data.isEmpty else { throw ProvisionError("no input on stdin") }
            guard let input = try? JSONDecoder().decode(Input.self, from: data) else {
                throw ProvisionError("stdin is not a JSON object with url and read_token")
            }
            let url = input.url.trimmingCharacters(in: .whitespacesAndNewlines)
            host = URL(string: url)?.host() ?? host
            // Read the stored item again: a Keychain error must not look like "none".
            let stored: Credentials?
            do { stored = try model.store.read() } catch {
                throw ProvisionError("cannot read the current settings: \(error.localizedDescription)")
            }
            if let stored, !stored.isSameServer(as: url), !replace {
                throw ProvisionError("settings for another server exist; launch with \(replaceFlag) to replace them")
            }
            // An admin token set in Settings stays for the same server.
            let admin = stored.flatMap { $0.isSameServer(as: url) ? $0.adminToken : nil }
            let new = Credentials(serverURL: url, readToken: input.readToken, writeToken: input.writeToken, adminToken: admin)
            // The write check first: save() checks the read token and saves.
            if !(input.writeToken ?? "").isEmpty {
                try await HeraldoClient(credentials: new).checkWriteToken()
            }
            _ = try await model.save(new)
            let write = (input.writeToken ?? "").isEmpty ? "no write token" : "write token present"
            log.notice("provision: saved for \(host, privacy: .public) in \(model.store.modeDescription, privacy: .public), \(write, privacy: .public)")
            await notifySaved()
        } catch {
            log.error("provision: failed for \(host, privacy: .public): \(error.localizedDescription, privacy: .public)")
        }
    }

    private static func stdinIsPipe() -> Bool {
        var st = stat()
        guard fstat(STDIN_FILENO, &st) == 0 else { return false }
        return (st.st_mode & S_IFMT) == S_IFIFO
    }

    private static func notifySaved() async {
        let content = UNMutableNotificationContent()
        content.title = "Heraldo settings were set from the command line"
        content.body = "If you did not do this, check the server and tokens in Settings."
        let request = UNNotificationRequest(identifier: "provisioned", content: content, trigger: nil)
        do {
            try await UNUserNotificationCenter.current().add(request)
        } catch {
            log.error("provision: notification: \(error.localizedDescription, privacy: .public)")
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
