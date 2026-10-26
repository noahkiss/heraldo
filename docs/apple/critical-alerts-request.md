# Critical Alerts entitlement request (draft)

Draft text for Apple's Critical Alerts entitlement request form. Not submitted.
Apple's form asks for the app, its purpose, and why the alerts must override
Do Not Disturb and the mute switch.

## App

- **Name:** Heraldo
- **Bundle ID:** com.nkmkdigital.heraldo
- **Developer:** NKMK Digital Co.
- **Distribution:** internal, through TestFlight. One household's devices.

## Request text

Heraldo is a self-hosted alerting app for a home's own infrastructure. A server
the owner runs collects events from monitoring systems at the home: a home
server, network equipment, backup jobs, and environmental sensors. Heraldo
delivers those events to the owner's iPhone.

Almost every event is passive or time-sensitive, and uses those interruption
levels. We ask for Critical Alerts for a small set of events where a missed
alert can cause harm or loss to people or property at the home, for example:

- a water leak sensor reports water,
- a smoke, carbon monoxide or radon sensor reports a dangerous level,
- a freezer or a pipe-freeze sensor reports a temperature out of range,
- the home's power or sump pump fails.

These events can happen at night, when the phone is muted or in a Focus mode.
A quick response prevents damage, or protects the people in the home.

Controls that keep Critical Alerts rare:

- The server marks an event critical only when its source and type are on an
  allowlist that the owner sets. No sender can mark an event critical by itself.
- Repeated events for one incident collapse into one alert, so a flapping sensor
  does not repeat a critical sound.
- The app asks for Critical Alerts permission separately, with an explanation,
  and the user can turn it off in Settings at any time.
- Everything else uses the time-sensitive level and the user's Focus settings.

The app has no ads, no accounts with third parties, and sends no data to anyone
other than the owner's own server.
