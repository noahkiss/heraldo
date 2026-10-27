import ActivityKit
import Foundation
import HeraldoKit

/// The app's side of Live Activities: it hands the server the push-to-start
/// token, and for each activity the server starts, its update token and its
/// end (docs/api.md, "Live Activities"). The server does the rest.
@MainActor
final class LiveActivities {
    static let shared = LiveActivities()

    private let defaults = UserDefaults.standard
    private var observed: Set<String> = []
    private var started = false

    /// The push-to-start token ActivityKit gave the app, hex. Sent with the
    /// device registration.
    private(set) var pushToStartToken: String? {
        get { defaults.string(forKey: "liveActivityToken") }
        set { defaults.set(newValue, forKey: "liveActivityToken") }
    }

    /// Reports that did not reach the server, by instance id. Sent again on
    /// the next foreground, so a lost token does not leave the server waiting
    /// and starting the activity a second time.
    private var pending: [String: InstanceReport] {
        get { defaults.data(forKey: "liveActivityReports").flatMap { try? JSONDecoder().decode([String: InstanceReport].self, from: $0) } ?? [:] }
        set { defaults.set(try? JSONEncoder().encode(newValue), forKey: "liveActivityReports") }
    }

    /// At launch, also when iOS wakes the app for a push-to-start.
    func start() {
        guard !started else { return }
        started = true
        Task {
            for await data in Activity<ProgressAttributes>.pushToStartTokenUpdates {
                let hex = APNs.hex(data)
                guard hex != pushToStartToken else { continue }
                pushToStartToken = hex
                log.info("live: push-to-start token received")
                await PushManager.shared.register(force: false)
            }
        }
        Task {
            for await activity in Activity<ProgressAttributes>.activityUpdates {
                observe(activity)
            }
        }
        for activity in Activity<ProgressAttributes>.activities {
            observe(activity)
        }
    }

    private func observe(_ activity: Activity<ProgressAttributes>) {
        guard observed.insert(activity.id).inserted else { return }
        let instance = activity.attributes.instance
        Task {
            for await data in activity.pushTokenUpdates {
                await report(instance, InstanceReport(token: APNs.hex(data)))
            }
        }
        Task {
            for await state in activity.activityStateUpdates {
                switch state {
                case .dismissed: await report(instance, InstanceReport(state: "dismissed"))
                case .ended: await report(instance, InstanceReport(state: "ended"))
                default: break
                }
            }
        }
    }

    private func report(_ instance: String, _ report: InstanceReport) async {
        var r = report
        r.device = PushManager.shared.deviceIdentifier
        // A newer report for the same instance carries what matters: keep a
        // token, take the latest state.
        if let old = pending[instance] {
            r.token = r.token ?? old.token
            r.state = r.state ?? old.state
        }
        do {
            try await AppModel.shared.client().reportInstance(id: instance, r)
            pending[instance] = nil
        } catch let e as HeraldoError where e.status == 404 {
            pending[instance] = nil // not one of the server's activities
        } catch {
            pending[instance] = r
            log.error("live: report for \(instance, privacy: .public) failed: \(error.localizedDescription, privacy: .public)")
        }
    }

    /// On foreground: send the reports that failed.
    func flush() async {
        for (instance, r) in pending {
            await report(instance, r)
        }
    }
}
