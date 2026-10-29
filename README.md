# Steam Auto-Updater

A tiny Windows app that detects installed Steam games and keeps them updated. It checks your library against Valve's public UpToDateCheck API every 30 minutes. When updates are pending, it starts Steam silently in the tray so the official client downloads them, then closes Steam again. No Steam password is stored or needed.

## What it does

1. Finds Steam via the registry (HKCU\Software\Valve\Steam), including extra libraries from libraryfolders.vdf.
2. Parses every appmanifest_*.acf to list installed games and their current build IDs.
3. Asks api.steampowered.com whether each build is current.
4. If games are stale and Steam is closed: launches steam.exe -silent, waits (up to 15 min), then closes Steam.
5. If Steam is already running: reports the pending updates and lets Steam handle them.

## Requirements

- Windows 10 or 11, 64-bit
- Steam installed and signed in at least once
- Nothing else. Single exe, no .NET, no installer.

## Deploy

1. Copy SteamAutoUpdater.exe to the Windows machine (anywhere, e.g. C:\Tools).
2. Double-click it. The first launch may show Windows SmartScreen because the exe is unsigned. Click "More info" then "Run anyway".

## How to verify it works (pass/fail)

1. Launch it. The window shows the Steam path at top and a list of every installed game with sizes. PASS if games appear. FAIL if the list is empty and the top line says "Steam not found".
2. Status column shows "Up to date" for current games. A game you know is current should read "Up to date". "Unknown" is normal for DLC or tools without a public build.
3. Click "Check now". The status bar shows "Checked at <time> — all N games up to date." or "N of M games need updates."
4. To watch it kick Steam: with Steam fully closed, uninstall nothing but wait for any game update (or use a game you know is out of date). The app starts Steam in the tray, the status bar counts down pending games, and when done it closes Steam (if "Close Steam when done" is checked).
5. Check "Start with Windows", reboot, and the app opens at login.

## Settings

- "Close Steam when done": closes Steam only when the app started it. It never closes Steam you opened yourself.
- "Start with Windows": writes HKCU\...\Run\SteamAutoUpdater. Remove with the checkbox or delete the value.
- Check interval is fixed at 30 minutes in v1.

## Build from source (from the Mac mini)

cd ~/Projects/steam-auto-updater
python3 make_icon.py
$HOME/go/bin/rsrc -manifest app.manifest -ico app.ico -o rsrc_windows_amd64.syso
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -H=windowsgui" -o SteamAutoUpdater.exe

## Limitations (v1)

- Honors Steam's own per-game update settings when it kicks Steam.
- Will not update anything while Steam is open in another sense: it never kills Steam it did not start.
- No tray icon yet; keep the window minimized and the 30-minute check keeps running.
- The UpToDateCheck API reflects public builds; beta-branch games may read "Update available" repeatedly.