import Foundation
import UserNotifications

/// Removes the delivered notifications a new push of the same key
/// supersedes: an incident's open alert when its resolve arrives, an earlier
/// failure of the same check. The server sends both with one
/// apns-collapse-id, but a push can carry another one (a bundle or a hold
/// forward), and then the system keeps both. On-call apps do the same.
public enum Superseded {
    /// One delivered notification: its request identifier and its userInfo.
    public struct Delivered {
        public var identifier: String
        public var userInfo: [AnyHashable: Any]

        public init(identifier: String, userInfo: [AnyHashable: Any]) {
            self.identifier = identifier
            self.userInfo = userInfo
        }
    }

    /// The identifiers of the delivered notifications `push` supersedes:
    /// those whose `heraldo.key` equals the push's key and whose
    /// `heraldo.id` differs. A push without a key supersedes nothing.
    /// `keeping` is the identifier of the push itself when it is already
    /// delivered (the app's `willPresent`), so it never removes itself.
    public static func identifiers(_ delivered: [Delivered], for push: PushInfo, keeping: String? = nil) -> [String] {
        guard let key = push.key, !key.isEmpty else { return [] }
        return delivered.compactMap { d in
            guard d.identifier != keeping, let info = PushInfo(userInfo: d.userInfo),
                  info.key == key, info.id != push.id else { return nil }
            return d.identifier
        }
    }

    /// Removes what `push` supersedes from Notification Center.
    public static func remove(for push: PushInfo, keeping: String? = nil,
                              center: UNUserNotificationCenter = .current()) async {
        guard let key = push.key, !key.isEmpty else { return }
        let delivered = await center.deliveredNotifications().map {
            Delivered(identifier: $0.request.identifier, userInfo: $0.request.content.userInfo)
        }
        let ids = identifiers(delivered, for: push, keeping: keeping)
        if !ids.isEmpty { center.removeDeliveredNotifications(withIdentifiers: ids) }
    }
}
