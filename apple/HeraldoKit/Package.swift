// swift-tools-version: 6.0
// HeraldoKit: the platform-neutral half of the app. The macOS and iOS app
// targets (and any extension) link this one package.
import PackageDescription

let package = Package(
    name: "HeraldoKit",
    platforms: [.macOS(.v15), .iOS(.v18)],
    products: [
        .library(name: "HeraldoKit", targets: ["HeraldoKit"]),
    ],
    targets: [
        .target(name: "HeraldoKit", swiftSettings: [.swiftLanguageMode(.v5)]),
        .testTarget(
            name: "HeraldoKitTests",
            dependencies: ["HeraldoKit"],
            swiftSettings: [.swiftLanguageMode(.v5)]
        ),
    ]
)
