# U64 Sync

U64 Sync is a slow, throttled, read-only mirror of an Ultimate64's SD card over its FTP server.
Never writes to the device.

- **Sync:** a crawler inventories the device (`MLSD`, size + mtime), a copier downloads new and
  changed files atomically (temp file, rename) into `data/current/<SD|Flash>/...`.
- **Deletions:** files that vanish on the device are removed locally after a grace period
  (default 7 days); a mass deletion is refused until approved in the UI.
- **Snapshots (optional):** hardlink trees of the mirror with last/daily/weekly/monthly retention.
- **Verification:** local files are re-hashed against the sha256 stored at download; bad or missing
  copies are downloaded again.
- **Device configuration (optional):** with `device_config.enabled`, the device's own settings are
  read from its REST API and saved as `u64-config.json` in the root of the mirror (snapshots
  include it; it is rewritten only when something changed). It can contain network and WiFi
  passwords, so it is written owner-only and the option is off by default.
- **Web UI:** dashboard, file browser, file download, folder zip. Optional basic auth.

## Run

    cp config.example.yaml config.yaml    # set device.host
    go run ./cmd/u64-sync          # reads ./config.yaml; use -config <file> for another one

Docker:

    mkdir -p data                         # bind mount; must be owned by you, not root
    cp config.example.yaml config.yaml
    echo "U64_UID=$(id -u)
    U64_GID=$(id -g)
    U64_WEB_PASSWORD=change-me" > .env
    docker compose up -d

The container runs as `U64_UID:U64_GID` (default 1000:1000) so it can write `./data`. Keep
`current/`, `snapshots/` and `tmp/` below that one directory: atomic rename and hardlinks need a
single filesystem.

Env overrides: `U64_HOST`, `U64_FTP_PASSWORD`, `U64_REST_PASSWORD`, `U64_WEB_USER`,
`U64_WEB_PASSWORD`, `U64_WEB_LISTEN`, `U64_DATA_DIR`.

Windows: unzip the release, edit `config.yaml`, run `u64-sync.exe -config config.yaml`. Device file
names Windows cannot store (`: ? * " < > |`, trailing dot/space, `CON`, `NUL`, ...) and `%` are saved
as `%XX` (e.g. `a?b.prg` becomes `a%3Fb.prg`); the web UI shows the real names. Store the data on
an NTFS volume (snapshots need hardlinks).

`cmd/u64-probe` inspects a device (FTP features, listing format, connection limit).

## Development

`make help` lists all tasks. The common ones: `make build`, `make test`, `make check` (what CI
runs: gofmt, tidy, vet for three platforms, race tests), `make dist` (release archives).
Tests run against an in-process fake Ultimate64 (`internal/fakeftp`), no device needed.

## License

MIT, see [LICENSE](LICENSE). Bundled Pico.css and htmx (both MIT) keep their licenses in `internal/web/static/`.
