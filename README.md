# Steam Auto-Updater

A tiny Windows app that detects installed Steam games and keeps them updated. It checks your library against Valve's public UpToDateCheck API every 30 minutes. When updates are pending, it starts Steam silently in the tray so the official client downloads them, then closes Steam again. No Steam password is stored or needed.

## What it does

1. Finds Steam via the registry (HKCU\Software\Valve\Steam), including extra libraries from libraryfolders.vdf.
2. Parses every appmanifest_*.acf to list installed games and their current build IDs.
3. Detects pending updates two ways: Steam's own manifest flag (exactly what the Steam client shows) plus Valve's public UpToDateCheck API for apps that support it.
4. If updates are pending: tells the running Steam client to download them immediately through its own UI API — no restart, no interruption (see "How forcing works" below).
5. If Steam is closed: the app starts it silently with the debug flag, forces the downloads, and can close it again when finished.
6. Optionally updates only inside a daily schedule window you set.

## Requirements

- Windows 10 or 11, 64-bit
- Steam installed and signed in at least once
- Nothing else. Single exe, no .NET, no installer.

## Deploy

1. Copy SteamAutoUpdater.exe to the Windows machine (anywhere, e.g. C:\Tools), or download it from the GitHub Releases page.
2. Double-click it. The first launch may show Windows SmartScreen because the exe is unsigned. Click "More info" then "Run anyway".

## How to verify it works (pass/fail)

1. Launch it. The window shows the Steam path at top and a list of every installed game with sizes. PASS if games appear. FAIL if the list is empty and the top line says "Steam not found".
2. Status column: "Up to date", "Update available" (detected via Valve API), "Update pending" (Steam's own flag — matches what the Steam client shows), or "Unknown" (no data available for that app).
3. Click "Check now". The status bar counts up ("Checking N of M…") and each row flips from Checking to its result as it completes. PASS if the count advances and rows settle on "Up to date" / "Update available" / "Unknown".
4. Schedule: check "Only update on a schedule". Set the window to a range that includes right now (e.g. 00:00 to 23:00). Within a minute the status bar returns to normal checking. PASS if checks keep happening.
5. Schedule pause: set the window to a range that excludes right now (e.g. 01:00 to 05:00 during daytime). Within one minute the status bar reads "Paused — outside schedule window (01:00–05:00)". PASS if it pauses and Steam never opens.
6. Typing "abc" into a window field and pressing Enter shows "Schedule times must be HH:MM (24h)" and does not save. PASS if bad input is rejected.
7. Restart the app after setting a schedule. The checkbox and times are restored. PASS if settings survive a restart (config file is %APPDATA%\SteamAutoUpdater\config.json).
8. To watch it kick Steam: with Steam fully closed, have any game update pending inside the schedule window. The app starts Steam in the tray, the status bar counts down pending games, and when done it closes Steam (if "Close Steam when done" is checked).
9. Check "Start with Windows", reboot, and the app opens at login.

## Schedule details

- Times are 24h HH:MM. A window may cross midnight (22:00 to 06:00).
- Start time is inclusive, end time is exclusive.
- Checks run every 30 minutes while inside the window. When the window opens, a check starts within a minute even if the last check was recent.
- Outside the window the app idles and shows "Paused" in the status bar. Pending updates show as "N update(s) waiting for window".
- "Check now" always checks and updates immediately, ignoring the schedule.
- If the window ends mid-update while the app started Steam, it still waits for the update to finish before closing Steam.

## How forcing works: the -cef-enable-debugging flag

Steam's client window is a Chromium app (steamwebhelper.exe). Steam ships with a built-in launch flag, `-cef-enable-debugging`, that turns on a developer port for that UI on localhost:8080. This app uses that port to run the exact call Steam's own Update button makes — `SteamClient.Downloads.QueueAppUpdate(appid)` — inside Steam's UI. The running client then cancels its update schedule for that game and starts downloading immediately. Steam is not restarted and not interrupted while the port is available.

What the app does:
- Always launches Steam with the flag whenever it starts Steam itself.
- Steam already running WITH the flag: pending updates are forced instantly, zero interruption.
- Steam already running WITHOUT the flag: the app restarts Steam once to enable it — only when updates are pending, never while a game is running.

To make your own Steam launches behave the same, add the flag to your Steam shortcut target:
`"C:\Program Files (x86)\Steam\steam.exe" -cef-enable-debugging`

Notes:
- The debug port accepts connections from the same machine only (localhost). Nothing is exposed to your network.
- `QueueAppUpdate` is an internal Steam UI call, not a documented Valve API. A future Steam update could change it; if forcing stops working, the app logs the queue errors and leaves updates to Steam.

## Settings

- "Close Steam when done": closes Steam only when the app started it. It never closes Steam you opened yourself.
- "Start with Windows": writes HKCU\...\Run\SteamAutoUpdater. Remove with the checkbox or delete the value.
- "Only update on a schedule" plus the window: stored in %APPDATA%\SteamAutoUpdater\config.json along with the Close-Steam preference.
- Check interval is 30 minutes inside the active window.
- "Force updates even if Steam is running" (default on): pending updates are forced immediately through Steam's own UI API when the debug port is up. If Steam is running without the flag, it is restarted once to get it (never during a game).
- Log file: %APPDATA%\SteamAutoUpdater\log.txt records every API check (game, result, milliseconds) plus Steam kick/monitor events. Include this file in any bug report.

## Build from source (from the Mac mini)

cd ~/Projects/steam-auto-updater
python3 make_icon.py
$HOME/go/bin/rsrc -manifest app.manifest -ico app.ico -o rsrc_windows_amd64.syso
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -H=windowsgui" -o SteamAutoUpdater.exe

## Limitations (v2.0)

- Never closes Steam it did not start.
- Forcing uses an internal Steam UI call (see the flag section); a Steam client update could change it.
- No tray icon yet; keep the window minimized and the schedule keeps running.
- Valve's UpToDateCheck API returns no data for many modern games; those rely on Steam's own manifest flag, so the app always agrees with what Steam shows. Beta-branch games on API-supported apps may read Update available repeatedly.
- Schedule is a single daily window (one start/end pair). Multiple windows per day are not supported yet.