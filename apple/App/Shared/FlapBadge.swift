import HeraldoKit
import SwiftUI

/// The "flapping" badge on an incident row: the key opens and resolves again
/// and again, so its pushes update one notification and do not page.
struct FlapBadge: View {
    let event: Event

    var body: some View {
        if let inc = event.incident, inc.isFlapping {
            Text("flapping")
                .font(.caption2.weight(.medium))
                .padding(.horizontal, 5)
                .padding(.vertical, 1)
                .foregroundStyle(.orange)
                .background(.orange.opacity(0.15), in: Capsule())
                .accessibilityLabel(label(inc))
        }
    }

    private func label(_ inc: IncidentInfo) -> Text {
        if let n = inc.flapCount, n > 0 {
            return Text("Flapping, \(n) opens")
        }
        return Text("Flapping")
    }
}

extension AppModel {
    /// Snoozes an incident's key for an hour: the server holds its pushes
    /// back and pushes it again then if it is still open.
    func snooze(_ e: Event) async throws {
        guard let key = e.key, !key.isEmpty else { return }
        let m = try await client().mute(MuteInput(key: key, for: PushCategory.snoozeFor, remind: true))
        log.info("snoozed \(key, privacy: .public) until \(m.until, privacy: .public)")
    }
}
