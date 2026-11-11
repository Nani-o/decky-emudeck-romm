# Downloading games

Press **Download** on a game, or **Download all missing** on a platform or collection, and the game
goes into the right EmuDeck folder, ready to play from ES-DE or to add to Steam with Steam ROM
Manager.

## The queue

The **Downloads** section of the panel shows what's queued and how far along each download is. Two
games download at a time. **✕** cancels a download, and **↻** retries one that failed.

Downloads survive restarts: if the Deck sleeps, reboots or loses its connection, the queue picks up
where it left off, resuming partly downloaded files when RomM still has them unchanged. A dropped
connection is retried a few times on its own before the download is marked failed.

While you're signed out of RomM, downloads wait until you sign in again.

## Where games go

Each RomM platform maps to its EmuDeck folder, such as `roms/snes` or `roms/psx`. A game downloads
into a staging folder first. The plugin completes transfers and extraction, validates target
collisions, then publishes the files. Publication and the ownership database are separate steps;
this does not promise an atomic whole-game rollback after a process or disk failure.

The plugin never overwrites a file it didn't download. If something is already where a game would
go, the download stops and says so, and the game shows as **On Deck (not managed)**.

### Install to

Some systems have several EmuDeck folders, such as `snes`, `snesna` and `sneshd`, or `arcade`,
`mame` and `fbneo`. A platform's list in the panel has an **Install to** choice for those. It
applies to new downloads; games already on the Deck stay where they are. Folders Steam ROM Manager
doesn't read are marked **not in Steam ROM Manager**.

## Archives

How a zip or 7z download is handled depends on the system:

- **Cartridge and computer systems** keep a zip or 7z holding a single game file as it is, since
  their emulators load it directly. Turn **Keep archives** off in the platform's list to extract
  every archive instead. The setting applies to new downloads.
- **Arcade systems** (MAME, FinalBurn Neo, Neo Geo, CPS, Model 2 and 3) and **DOS** always keep
  their archives: the emulator needs them as they are.
- **Disc and modern systems** (PlayStation, Dreamcast, GameCube, Wii, Switch, 3DS and others) are
  always extracted.

Switch also supports RAR archives and the qualified multipart base-game sets. Missing volumes,
encryption, unsafe paths and colliding output files are refused before publication.

## Games made of several files

A game RomM keeps as several files, such as a multi-disc game, is laid out the way its emulator and
ES-DE expect:

- **Disc-based and classic computer systems** get a `<Game>.m3u` folder holding the discs and a
  playlist the plugin writes. ES-DE shows the folder as one game, and the emulator swaps discs from
  the playlist.
- **PlayStation 3** disc layouts go in a `<Game>.ps3` folder. A PSN PKG still requires native RPCS3
  installation; downloading or extracting the package does not make it a bootable game.
- **Switch** keeps the base game, updates and DLC under one title entry. A single raw base uses a
  layout such as `Game.nsp/Game.nsp`, which ES-DE interprets as one game. Extras are kept under
  `update/` and `dlc/`, with owned `noload.txt` markers to exclude them from the ES-DE game list.
  Eden needs its native recursive game-directory scan and external-content option enabled.
- **Wii U** games go in a `<Game>` folder, as Cemu reads them.
- **DOS** games are zipped into `<Game>.zip`, which DOSBox Pure mounts as a drive.

Other systems can't place a game of several files, and say so.

## Formats RomM converts

Some formats don't play as they are on the Deck. For those, the plugin asks RomM for a format that
does:

| System | Stored as      | Downloaded as  |
| ------ | -------------- | -------------- |
| 3DS    | `.cia`         | `.cci`         |
| Switch | `.nsz`, `.xcz` | `.nsp`, `.xci` |
| Xbox   | `.iso`         | XISO `.iso`    |

If RomM can't convert the file, it downloads as stored, and the game's page shows **⚠ Not ready to
play** with what's left to do, such as installing a `.cia` in Azahar.

Switch downloads include files RomM categorizes as `game`, `update` and `dlc`. Other systems retain
the upstream base-game policy. Manuals, soundtracks and unrelated categories are not downloaded.
Files outside a title's indexed manifest cannot be fetched as its extras.

## Disk space

Before each download, the plugin checks there's room for it and for the downloads ahead of it,
keeping some space free. Archives that get extracted need room for both copies while they unpack.

When the drive is full, the queue goes **⚠ On hold** and waits. It checks again every half minute,
and right away when you delete a game or cancel a download.

**Download all missing** checks the space first, and offers **Queue what fits** when not everything
does.

## Waiting for games to close

With **Wait for games to close** on (Settings), a finished download waits to be extracted while an
emulator is running, so your game doesn't stutter. Other downloads keep going meanwhile.

## BIOS files

A platform's list and its games' pages show its BIOS status:

- **⚠ BIOS missing**, with **Download BIOS** to fetch the missing files from RomM.
- **⚠ Different BIOS**: a different file is already there. The plugin leaves it as it is.
- **All present**.

With **Download BIOS with games** on (Settings), a platform's first game download also fetches its
missing BIOS files. Files already in the BIOS folder are never replaced.

Switch, PS3, PS4, 3DS and Wii U keep their firmware inside their emulators; install it from there.

## ES-DE

### Art

With **Add art to ES-DE** on (Settings), downloaded games get their RomM cover and first screenshot
in ES-DE without scraping. Art already in ES-DE is never replaced, and art ES-DE's scraper replaced
stays when you delete the game.

### Rescanning

ES-DE reads its ROM folders when it starts. When games come or go while ES-DE is open, the plugin
reminds you to run **Utilities → Rescan ROM directory** in ES-DE, or restart it, to see them.
