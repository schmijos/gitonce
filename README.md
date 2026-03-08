# gitonce

Upload a zip file, get back a one-time-use git repository URL.

## How it works

1. Upload a zip via the web UI or API — the server stores it and returns a git URL.
2. Clone from that URL. The server builds a git repository from the zip contents entirely in memory and serves it over the Smart HTTP protocol.
3. The URL expires after one clone. The zip is deleted from disk once the download completes.

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
make run
```

Listens on `:8080`.
