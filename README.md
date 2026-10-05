# Wild Link

Wild Link is a serverless office messenger for Windows and Mac computers on the same network (Wi-Fi or LAN cable). No server, no internet, no accounts. Windows and Mac users chat with each other normally.

## Windows
1. Run `WildLink-Setup.exe` on each PC (needs admin rights once). It installs to `C:\Program Files\WildLink`, adds Start menu and desktop shortcuts, opens the Windows Firewall for Wild Link on every network type, and registers it in *Settings > Apps*.
2. Setup closes any running LAN Chat or WildChat first, and your old settings, history, stickers and GIFs are carried over automatically.
3. Wild Link puts an icon in the notification area: click it to open the chat, right-click for Open/Quit, and new messages arrive as notifications there.

## Mac
1. Unzip `WildLink-mac.zip` and drag **Wild Link** into **Applications**.
2. First launch: right-click it, choose **Open**, then **Open** again. On macOS 15 or later, try once, then allow it under **System Settings > Privacy & Security > Open Anyway**. If nothing happens at all, run `xattr -dr com.apple.quarantine /Applications/WildLink.app` in Terminal.
3. Allow incoming network connections if macOS asks. Messages arrive in Notification Centre.
4. It runs in the background with no Dock icon; reopen from Launchpad or Spotlight, and quit from Settings.

- Files go to `Downloads\WildLink Files` (`~/Downloads/WildLink Files` on Mac); your sent copies are in the `Sent` subfolder.
- Settings and history: `%APPDATA%\WildLink` on Windows, `~/Library/Application Support/WildLink` on Mac.

## File transfers
Files stream straight from one computer to the other with no size limit. A transfer that is
cut off (Wi-Fi drop, laptop lid, the other person quitting) resumes from where it stopped
rather than starting again: the part-finished file is kept and the sender retries up to four
times, asking the receiver how much it already has. A file only appears in the chat once it
is complete, so nobody opens a half-copied video. The receiver refuses a file it has no room
for, with a clear message, and part files older than a day are cleaned up automatically.
The progress bar shows percentage, speed and time remaining on both sides.

## Updates
Push a tag and GitHub Actions builds both installers and publishes a release; every copy of
Wild Link sees an update bar within hours. Settings also accepts a web address or a shared
folder instead. See `update-kit/README.md`.

## Using it
- **Everyone** goes to all online colleagues; click a person for a private chat.
- **Deleting**: hover a message, click the ⋯ button, then *Delete for me* (hides it on your computer) or *Delete for everyone* (withdraws it from colleagues' chats, where it shows as "This message was deleted"). Only the sender can delete for everyone.
- **Notifications** come from the operating system, so they work even when the chat window is closed. They're skipped while the window is in front.
- **Photos and videos** show inline in the chat. Send them with the photo button, drag them into the window, or paste a screenshot with Ctrl+V. Click a photo to view it full size; videos play in place.
- **Stickers**: the smiley button opens the Wild Link sticker pack (Chai break?, On it!, Done!, LGTM and more). Add your own under *My stickers*, or click **Save as sticker** on any photo in the chat. Built-in stickers work fully offline.
- **GIFs**: the GIFs tab searches GIPHY (needs internet on the sender's computer and a free API key from developers.giphy.com, pasted once in the GIFs tab). The sender's WildLink downloads the GIF and sends the file, so colleagues without internet still see it. *My GIFs* keeps your favourites offline: add .gif files, or click **Save to My GIFs** on any GIF in the chat.
- **Any other file** (PDF, Excel, ZIP…) goes through the paperclip button or drag and drop.
- Everything received is saved to `Downloads\WildLink Files`; your own sent copies go to `Downloads\WildLink Files\Sent`.
- Closing the window keeps WildLink running in the background, and it pops open when a message arrives. Use **Quit WildLink** to stop it fully.
- If someone doesn't appear (different subnet, or broadcast blocked), use **Add someone by IP address**.

Videos play inline if they're MP4 (H.264), WebM or most phone MOV files; anything else shows an **Open** button to play it in the PC's video player.

## Troubleshooting
- **Nobody shows up:** the network is probably set to *Public* in Windows. Set it to *Private*, or allow `WildLink.exe` in Windows Defender Firewall (TCP 47821, UDP 47822).
- **Works on LAN but not Wi-Fi:** turn off "AP/client isolation" on the router, or use Add by IP.
- **SmartScreen warning:** the exe isn't code-signed. Click *More info → Run anyway*, or sign it with your company certificate.
- Settings, history and log live in `%APPDATA%\WildLink`.

## How it works
- Discovery: UDP broadcast on port 47822 every 3 s (plus unicast to manually added IPs).
- Messages and files: direct HTTP between peers on TCP 47821. Files stream straight from PC to PC with no size limit.
- UI: an embedded web page shown in a borderless Edge window, served only to localhost.

Traffic is not encrypted and senders are not authenticated, so use it on a trusted office network.

## Where shared files go
- Sender: the file is copied to `Downloads\WildLink Files\Sent` and a progress bar shows while it goes out.
- Receiver: nothing to click. The file streams straight to their PC, a "Receiving…" progress bar shows above the message box, and when it finishes it appears in the chat (photos and videos play inline) and is saved in `Downloads\WildLink Files`.
- The receiver's WildLink must be running. If their window is closed but the app is running, the file still arrives and the window pops up.

## Building
Requires Go 1.22+. The icon is embedded via `rsrc_windows_amd64.syso` (regenerate with `rsrc -ico wildlink.ico -arch amd64 -o rsrc_windows_amd64.syso`).
```
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-H windowsgui -s -w" -o WildLink.exe .
makensis installer.nsi        # produces WildLink-Setup.exe
```
Change the publisher name shown in *Settings > Apps* at the top of `installer.nsi`.

To give everyone GIF search without each person entering a key, bake a GIPHY key into the build:
```
go build -ldflags "-X main.defaultGiphyKey=YOUR_KEY ..."
```

Mac (both Apple Silicon and Intel, from any OS):
```
GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o WildLink.app/Contents/MacOS/wildlink-arm64 .
GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o WildLink.app/Contents/MacOS/wildlink-amd64 .
```
plus `mac-launcher.sh` as `Contents/MacOS/WildLink`, `Info.plist.mac` as `Contents/Info.plist`, and `wildlink.icns` in `Contents/Resources`.
