# Nani fork of EmuDeck RomM Sync

This fork keeps EmuDeck and ES-DE as the emulator setup and game frontend. It extends the upstream
download pipeline to manage Switch game files, updates and DLC together while retaining their RomM
categories and protecting local user-managed files and saves.

The fork starts from upstream version 0.5.0. Its first implementation is being validated; the
platform coverage and installation steps below remain the upstream baseline until each added path
has passed its runtime checks.

Upstream project: [chenasraf/decky-emudeck-romm](https://github.com/chenasraf/decky-emudeck-romm).
The original authors and GPL-3.0 license are retained.

---

<p align="center">
  <img src="assets/banner.svg" alt="EmuDeck RomM Sync" width="100%">
</p>

# EmuDeck RomM Sync

Browse your [RomM](https://romm.app) library from your Steam Deck, download games straight into
[EmuDeck](https://www.emudeck.com)'s folders, and keep your saves in sync with RomM, all from the
Quick Access Menu, without leaving Gaming Mode.

## What it does

- **Your whole RomM library on the Deck.** Browse by platform or collection, search by title, and
  open a full-screen library with covers.
- **One-press downloads.** Games land in the right EmuDeck folder, extracted or kept zipped as the
  system needs, multi-disc games bundled into a single playlist entry, ready for ES-DE and Steam ROM
  Manager.
- **BIOS files included.** Missing BIOS for a platform download with its first game.
- **Save sync.** Saves from RetroArch, melonDS, mGBA, RMG and DuckStation upload to RomM as you play
  and download on your other devices. Conflicts are never resolved behind your back, and overwritten
  saves are kept as backups. Game Boy saves convert between emulators, cartridge clock included.
- **Save state sync.** Game Boy, Game Boy Color and Game Boy Advance save states sync with RomM both
  ways, so a game started in RomM's web player picks up on the Deck, and back.
- **Updates.** When a game's files change on RomM, the plugin offers the update, and your save
  follows the game even when its name changes.
- **Games you already have.** Find my games matches the ROMs already on your Deck against RomM, so
  the plugin can manage them or sync their saves.
- **Safe by design.** The plugin never replaces or deletes a file it didn't download, and never
  deletes a save RomM doesn't hold.

## Requirements

- A Steam Deck (or another SteamOS/Linux handheld) with [Decky Loader](https://decky.xyz).
- [EmuDeck](https://www.emudeck.com), set up with ES-DE.
- A [RomM](https://romm.app) server, version 5.1.0 or newer, reachable from the Deck.

## Installing this fork

There is no published fork release yet. The Homelab deployment uses the immutable upstream 0.5.0
release as its package base, with the frontend and backend built from this fork. Its installer pins the fork
commit, source archive, Go compiler and Linux binary, and preserves the existing device identity,
settings and game database.

For a source build, follow [DEVELOPMENT.md](DEVELOPMENT.md) and package this checkout with
`make package`. Installing an upstream release alone does not install this fork's additions. The
default RPC protocol is unchanged; the backend version is `0.5.0-nani.2`.

Switch downloads select the latest applicable update by its numeric package version and title ID,
while keeping all DLC. Older updates remain available on RomM and are omitted from the Deck
manifest. Updating an existing managed game removes superseded owned files without downloading
unchanged base files again. Canonical `[TITLE_ID][vVERSION]` filename tags identify packages;
unidentified or ambiguous update sets stop before installation instead of choosing arbitrarily.
The download queue reports the selected transfer size and file count, then shows separate
extraction and installation phases. Cancelling extraction stays pending until it actually stops;
the short final file placement cannot be cancelled.

Switch extras are stored under each game's `update` and `dlc` directories. The fork writes ES-DE
`noload.txt` markers there; Eden needs its native recursive game-directory scan and external-content
option enabled. Save synchronization uses an explicit Eden NAND/profile/title binding. Games with no
qualified title binding keep local saves; they do not acquire a guessed save mapping. Invalid
bindings and sync conflicts are reported separately. These additions require runtime validation
before being treated as supported on a particular device.

The upstream [getting started guide](docs/getting-started.md) explains the original setup and
sign-in flow; its release download links install the stock upstream plugin.

## Documentation

- [Getting started](docs/getting-started.md): installing, connecting to RomM, signing in
- [Browsing the library](docs/library.md)
- [Downloading games](docs/downloads.md): where games go, archives, multi-disc games, BIOS, ES-DE
- [Managing games](docs/managing-games.md): deleting, updates, Find my games, games you already had
- [Save sync](docs/save-sync.md): supported emulators, conflicts, slots, backups, save states
- [Settings and plugin data](docs/settings.md)
- [FAQ and troubleshooting](docs/faq.md)

## Contributing

Bug reports and pull requests are welcome. [DEVELOPMENT.md](DEVELOPMENT.md) explains how the plugin
is built and how to run it locally.

## License

[GPL-3.0](LICENSE)
