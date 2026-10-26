# heraldo

The homelab's notification hub, and later its single pane of glass.

- **Now:** one place every notification enters, gets stored, and is delivered to
  Noah's devices through an iOS app we own (internal TestFlight).
- **Features in scope:** levels from passive to time-sensitive (critical alerts if
  Apple grants the entitlement), threads and icons per source, failure-to-recovery
  incidents, silent digests with full text on tap, action buttons, images and links,
  Live Activities for long-running state (prints, backups), dead-man's-switch
  check-ins, a history, and an inbox-style CLI.
- **Later:** a homelab status overview built on the same event stream.

Status: planning. The plan and its stages live in bm (project `heraldo`).
