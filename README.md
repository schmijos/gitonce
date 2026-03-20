# gitonce

Upload a zip file, get back a one-time-use git repository URL.

## How it works

1. Upload a zip via the web UI or API — the server stores it and returns a git URL.
2. Clone from that URL. The server builds a git repository from the zip contents entirely in memory and serves it over the Smart HTTP protocol.
3. The URL expires after one clone. The zip is deleted from disk once the download completes.

```mermaid
sequenceDiagram
      autonumber
      actor Coder
      participant Nctl as nctl CLI
      participant GitOnce as gitonce
      participant Deploio as deplo.io (kpack)

      Coder->>Nctl: nctl create app DEMO_APP --from-local-dir=./
      Nctl->>GitOnce: /upload ZIP
      GitOnce-->>Nctl: {"commit": COMMIT, "url": REPO_URL }
      Nctl->>Nctl: nctl create app DEMO_APP --git-url=REPO_URL --git-revision=COMMIT
      Nctl->>Deploio: Nine API Call

      Deploio->>Deploio: Start build from Git source
      Deploio->>GitOnce: GET REPO_URL/info/refs?service=git-upload-pack
      GitOnce-->>Deploio: Advertise refs

      Deploio->>GitOnce: POST REPO_URL/git-upload-pack
      GitOnce-->>Deploio: Send repository pack
      GitOnce->>GitOnce: Expire one-time repo and delete ZIP

      Deploio->>Deploio: Build and deploy app
      Coder->>Nctl: nctl logs build demo-app-1
      Nctl->>Deploio: Nine API Call
      Deploio-->>Nctl: Successful build logs

    Note over Coder,Nctl: Later operations using same Git URL will fail, for example:
      Coder->>Nctl: nctl update app DEMO_APP --git-url=REPO_URL
      Nctl->>Deploio: Request redeploy with same Git URL
      Deploio->>Deploio: Try to fetch source again
      Deploio->>GitOnce: GET/POST Git fetch
      GitOnce-->>Deploio: 410 Gone
      Coder->>Nctl: nctl logs build demo-app-2
      Nctl->>Deploio: Nine API Call
      Deploio-->>Nctl: Failed build logs
```

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
