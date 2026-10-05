# Releasing updates through GitHub

Everyone running Wild Link watches one GitHub repository. When you publish a release
there, a bar appears at the top of their chat — "Wild Link 2.2.0 is available — <your
release note>" — with an **Update now** button that downloads the right installer for
their computer and opens it.

## One-time setup

1. Create a repository (private or public) and push this source into it.
2. On each computer: **Settings → Updates →** type `your-org/wildlink` → **Save**.
   A full link like `https://github.com/your-org/wildlink` works too.
   Builds made by the included workflow already point at their own repository, so
   anyone who installs from a release never has to set this.
3. Private repositories: GitHub requires a token to read releases, which Wild Link
   doesn't send, so use a public repository, or keep the code private and publish the
   releases in a small public one.

## Publishing a new version

1. Raise the version in three places: `appVersion` in `main.go`, `VERSION` in
   `installer.nsi`, and both version keys in `Info.plist.mac`.
2. Commit, then tag and push:

       git tag v2.2.0
       git push origin v2.2.0

3. GitHub Actions (`.github/workflows/release.yml`) builds `WildLink-Setup.exe` and
   `WildLink-mac.zip`, runs the tests, checks that the tag matches `appVersion`, and
   publishes the release with both files attached. It takes about three minutes.

That's the whole job. Everyone is told within six hours, immediately if they restart
Wild Link, and on demand via **Check now** in Settings.

## How the app reads a release

- The tag is the version: `v2.2.0` means 2.2.0, and it must be higher than what people
  are running, or nothing happens.
- The first line of the release notes is what people see in the update bar, so write it
  for colleagues: "Faster file transfers", not a commit list.
- Attachments are matched by name: the `.exe` goes to Windows, the file with "mac" in
  the name goes to Macs.
- Drafts are ignored, so you can prepare a release without anyone seeing it.

## Without GitHub

Settings also accepts a web address or a shared folder holding an `update.json`:

    { "version": "2.2.0", "notes": "Faster file transfers",
      "windows": "WildLink-Setup.exe", "mac": "WildLink-mac.zip" }

The two installers sit next to that file. `update.json` in this folder is a template.

## If you don't set any of this up

Wild Link still notices when a colleague is running a newer version and says so in the
update bar, so nobody is left behind silently.
