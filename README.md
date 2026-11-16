# Bloem Bookwarehouse ebook requests plugin

> [!CAUTION]
> **Offered without support.** Bookwarehouse ebook requests is provided as-is, with no support of any kind. Issues are turned off, questions go unanswered, and pull requests are not accepted: they are closed without review. You are free to fork it under the AGPL v3.

A plugin for [Bloem Server](https://github.com/Bloem-Studios/bloem-server). When a viewer asks for an ebook by its ISBN and the request is approved, the plugin orders the book from a Bookwarehouse server, reports how the order is going, and can test the connection. It is for administrators who already run Bookwarehouse and want Bloem's ebook requests to feed it.

Bloem keeps everything else: who may ask, approval, request limits, and deciding that a request is done. A request is done only when an EPUB or PDF with that ISBN shows up in the linked Bloem ebook library. Bookwarehouse reporting "completed" is not enough.

The plugin does not search by title or author, does not fetch audiobooks, and never cancels or deletes anything at Bookwarehouse.

## Install

The plugin is distributed through the [Bloem plugin catalog](https://github.com/Bloem-Studios/bloem-plugins). Releases are built for Linux (amd64 and arm64) and macOS (arm64).

You need:

1. A Bookwarehouse server that Bloem Server can reach over HTTPS (plain HTTP only on a network you trust), and an API key for it.
2. The Bookwarehouse storage plugin (`bloem.storage.bookwarehouse`), a Bookwarehouse storage source, and an enabled ebook library that reads from it. The library must belong to the default organization, or be a platform library granted to it. Native storage needs its own approval in Bloem; see the catalog's [Bookwarehouse installation guide](https://github.com/Bloem-Studios/bloem-plugins/blob/main/docs/bookwarehouse-installation.md).
3. A platform administrator account that is an active member of the default organization.

Then, in Bloem's admin console:

1. Open **Plugins**. If the catalog is not listed, choose **Add repository** and enter
   `https://raw.githubusercontent.com/Bloem-Studios/bloem-plugins/main/manifest.json`. Install **Bookwarehouse ebook requests**.
2. Open **Requests**, choose the **Ebooks** tab, and find the **Ebook fulfillment** panel.
3. Under **Add Bookwarehouse connection**, pick the plugin, name the connection, and fill in the base URL and API key (see the table below). Choose **Add connection**.
4. Choose the **Library** the connection delivers to, then **Save library**. Only enabled Bookwarehouse ebook libraries that the default organization can use are listed.
5. Choose **Test connection**. Success means the address and key work: the test only reads Bookwarehouse's order list, so it does not prove Bookwarehouse can find or download books.
6. Tick **Enable ebook requests** and choose **Save ebook request policy**. Ebook requests are off by default, and only profiles in the default organization can ask.

Each installation holds one connection. The plugin refuses a second one.

## Configuration

Connection values, entered when you add the connection:

| Field | Required | Notes |
| --- | --- | --- |
| Base URL | yes | Bookwarehouse's address, for example `https://books.example.com`, or with a reverse-proxy path such as `https://example.com/bookwarehouse`. The plugin adds `/api/v1` itself. HTTPS only, unless **Allow HTTP** is on. No user info, query string, fragment or `..` segments. |
| API key | yes | 1 to 8,192 printable ASCII characters, no spaces. Bloem stores it encrypted and passes it on each call. The plugin does not keep it. |

Connection settings (manifest config key `connection`):

| Setting | Default | Meaning |
| --- | --- | --- |
| `allow_http` | `false` | Permit plain HTTP. Shown as **Allow HTTP**. Use it only on a network you trust. |
| `allow_pdf` | `false` | Permit PDF as the requested format. |
| `preferred_format` | `epub` | `epub`, or `pdf` when `allow_pdf` is `true`. |

Bloem's ebook setup creates EPUB connections and has no PDF switch, so in practice requests are for EPUB unless the stored settings are changed.

Other behaviour worth knowing:

- Each order, status check and connection test gives up after 30 seconds.
- The plugin never follows redirects, so the API key is never sent to another address. It does not pass on Bookwarehouse's error messages.
- It reads at most 10,000 entries of Bookwarehouse's order list. With more, it refuses to order and the connection test fails. For books it reads everything up to 10,000 in the connection's format; beyond that it reads only the first 100 and relies on Bookwarehouse answering "already have it" at order time.
- The plugin honours `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY`.

How a request is handled, what the plugin does when it cannot tell whether an order arrived, the status mapping and the error messages are in [docs/behaviour.md](docs/behaviour.md).

## Build and test

You need Go; `go.mod` declares the minimum version and toolchain.

```sh
go build -o bloem-plugin-requests-bookwarehouse ./cmd/bloem-plugin-requests-bookwarehouse
go vet ./...
go test -race ./... -count=1
```

The plugin is built on the public [`github.com/Bloem-Studios/bloem-plugin-sdk`](https://github.com/Bloem-Studios/bloem-plugin-sdk) (Apache-2.0). The tests use only synthetic `httptest` servers and credentials. The version is set in `cmd/bloem-plugin-requests-bookwarehouse/main.go` and in `manifest.json` next to it, and a test keeps the two equal.

## Security

Report vulnerabilities privately through GitHub's private vulnerability reporting: <https://github.com/Bloem-Studios/bloem-plugin-requests-bookwarehouse/security/advisories/new>. Please do not open public issues for security problems.

## Licence

Licensed under the [GNU Affero General Public License v3](LICENSE). Releases published before this licence change were published under the Bloem Studios Proprietary Plugin License and stay under it.
