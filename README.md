# gitonce

Upload a zip file, get back a one-time-use git repository URL.

## How it works

1. Upload a zip via the web UI or API — the server stores it and returns a git URL.
2. Clone from that URL. The server builds a git repository from the zip contents entirely in memory and serves it over the Smart HTTP protocol.
3. The pack can be fetched once. The zip is deleted from disk once the download completes. Ref advertisement (`ls-remote`) keeps working afterwards, only the fetch returns `410 Gone`.

Repositories live in process memory and `/tmp`, so run exactly one replica and expect a restart between upload and clone to lose pending uploads.

## API

**Upload**

```
POST /upload
Content-Type: multipart/form-data
Field: zipfile
```

Response:

```json
{
  "message": "upload successful",
  "url": "https://example.com/gitonce/1234567890-abcdef01.git"
}
```

**Clone**

```
git clone https://example.com/gitonce/1234567890-abcdef01.git
```

Standard git Smart HTTP — works with any git client.

## Running

```
make test
make lint
make run
```

Listens on `:8080`.

`make test` also runs the nctl contract test in `nctlcontract/`, which builds the binary and drives it with the real nctl client (`github.com/ninech/nctl/api/gitonce`) followed by a shallow go-git clone. Set `SKIP_NCTL_CONTRACT=1` to skip it. Bump the nctl version in `nctlcontract/go.mod` when the client changes.
