import Foundation

/// A Darwin notification from the notification extension to the running
/// app: "a push came in, sync". It carries no data and costs nothing when
/// the app is not running.
public enum SyncSignal {
    /// The name for an app bundle ID.
    public static func name(appBundleID: String) -> String { appBundleID + ".sync" }

    /// The Info.plist key an extension fills with its app's bundle ID.
    public static let appBundleIDInfoKey = "HeraldoAppBundleID"

    /// The name an extension posts, from its Info.plist.
    public static func extensionName(_ bundle: Bundle = .main) -> String? {
        guard let id = bundle.object(forInfoDictionaryKey: appBundleIDInfoKey) as? String,
              !id.isEmpty, !id.hasPrefix("$(") else { return nil }
        return name(appBundleID: id)
    }

    public static func post(_ name: String) {
        CFNotificationCenterPostNotification(CFNotificationCenterGetDarwinNotifyCenter(),
                                             CFNotificationName(name as CFString), nil, nil, true)
    }

    /// Calls `body` on the main queue each time `name` is posted, until the
    /// returned observer is released.
    public static func observe(_ name: String, _ body: @escaping @Sendable () -> Void) -> Observer {
        Observer(name: name, body: body)
    }

    public final class Observer: @unchecked Sendable {
        let body: @Sendable () -> Void
        let name: String

        init(name: String, body: @escaping @Sendable () -> Void) {
            self.name = name
            self.body = body
            let me = Unmanaged.passUnretained(self).toOpaque()
            CFNotificationCenterAddObserver(CFNotificationCenterGetDarwinNotifyCenter(), me, { _, observer, _, _, _ in
                guard let observer else { return }
                let o = Unmanaged<Observer>.fromOpaque(observer).takeUnretainedValue()
                DispatchQueue.main.async { o.body() }
            }, name as CFString, nil, .deliverImmediately)
        }

        deinit {
            CFNotificationCenterRemoveObserver(CFNotificationCenterGetDarwinNotifyCenter(),
                                               Unmanaged.passUnretained(self).toOpaque(),
                                               CFNotificationName(name as CFString), nil)
        }
    }
}
