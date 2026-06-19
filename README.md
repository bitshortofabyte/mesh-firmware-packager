# mesh-firmware-packager

Go tool to snapshot the **latest stable firmware** from Meshtastic and MeshCore (meshcore.io) GitHub repos into convenient zip archives + JSON manifest.

## What it does

- Queries GitHub Releases API for each project.
- Selects only **non-draft, non-prerelease** releases (skips Meshtastic's frequent Alphas automatically).
- For MeshCore, intelligently groups the coordinated `companion-vX.Y.Z` / `repeater-vX.Y.Z` / `room-server-vX.Y.Z` releases (published ~same time) so you get all three roles in one package.
- Filters assets to firmware files only (`.bin`, `.uf2`, `.elf`, `.hex`, `.dfu`, some `.zip`) while excluding source tarballs, checksums, etc.
- Downloads them (with progress), computes SHA256 on the fly.
- Produces one zip per project:
  - `meshtastic-firmware-2-7-15-567b8ea.zip`
  - `meshcore-firmware-1-16-0.zip`
- Each zip contains:
  - All matching firmware binaries (flat)
  - `manifest.json` with version, tags, publish date, every file's name/size/SHA256/original download URL, total stats.

## Quick start

```bash
cd mesh-firmware-packager

# Recommended: use a token to avoid rate limits (especially with Meshtastic's 100-300+ assets)
export GITHUB_TOKEN=ghp_your_token_here

go run main.go
# or
go run main.go -out /path/to/your/firmware/collection
```

Output example:

```
=== Processing Meshtastic (meshtastic/firmware) ===
Latest stable version group: 2.7.15.567b8ea (includes 1 release(s))
Found 187 firmware assets (total ~92.4 MB)
  [  1/187] firmware-tbeam-2.7.15.567b8ea.uf2 (0.8 MB)
  ...
✓ Created /home/you/meshtastic-firmware-2-7-15-567b8ea.zip (187 files, 92.4 MB)

=== Processing MeshCore (meshcore-dev/MeshCore) ===
Latest stable version group: 1.16.0 (includes 3 release(s))
Found 48 firmware assets (total ~18.2 MB)
...
✓ Created /home/you/meshcore-firmware-1-16-0.zip (48 files, 18.2 MB)
```

## manifest.json example (excerpt)

```json
{
  "project": "Meshtastic",
  "version": "2.7.15.567b8ea",
  "tags": ["v2.7.15.567b8ea"],
  "release_url": "https://github.com/meshtastic/firmware/releases/tag/v2.7.15.567b8ea",
  "published_at": "2025-11-19T...",
  "generated_at": "2026-06-19T...",
  "total_files": 187,
  "total_size_bytes": 96912345,
  "firmware_files": [
    {
      "name": "firmware-tbeam-2.7.15.567b8ea.uf2",
      "size": 812345,
      "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
      "download_url": "https://github.com/.../releases/download/v2.7.15.567b8ea/firmware-tbeam-2.7.15.567b8ea.uf2",
      "content_type": "application/octet-stream"
    },
    ...
  ]
}
```
