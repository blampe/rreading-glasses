# 🤓 rreading-glasses

Corrective lenses for curmudgeonly readars in your life.

This is a drop-in replacement for R—'s metadata service. It works with your
existing R— installation, it's backwards-compatible with your library, and it
takes only seconds to enable or disable. You can use it to permanently manage
your library, or to temporarily help you add works the R— service doesn't have
yet.

Unlike R—'s proprietary service, this is much faster, handles large authors,
and doesn't take months to load new works. It is also intended to be
self-hostable and will be kept (mostly) open source (see "Getters" below).

> [!IMPORTANT]
> This is not an official project and is still very much in progress. Reach out
> to me directly if you have questions or need help, please don't bother the R—
> team.

```mermaid
graph LR;
    R[R—]-.->M[official metadata];
    R-->rreading-glasses;

    classDef dotted stroke-dasharray:2,text-decoration:line-through;
    class M dotted;
```

## Key differences

Please read this section closely before deciding to use the service, especially
if you intend to use it permanently. The service works well for my personal use
case, but there are some subtle differences that might impact you.

### Functional changes

I have deviated slightly from the official service's behavior to make a couple
of, in my opinion, quality of life improvements. These aren't due to technical
limitations and can be changed, so I'm eager to hear if people think these are
an improvement or if it would be better to match the official behavior more
exactly.

- Book titles no longer include subtitles (so `{Book Title}` behaves like
  `{Book TitleNoSub}` by default). This de-clutters the UI, cleans up the
  directory layout, and improves import matching but __you may need to
  re-import some works with long subtitles__. I think the trade-off is worth it
  but others might disagree -- let me know!

- __Adding a new author now only adds (up to) 20 of their works instead of
  their entire library__. Originally this didn't automatically add _any_ works
  for new authors, because I personally much prefer a "book-based" workflow
  over the default "author-based" behavior. I figure adding 20 works is a sort
  of compromise between the two. Tell me if you really need the default (add
  all books) behavior, or if you would prefer to keep it entirely book-based!

### Not implemented yet

- __Translated works aren't handled well at the moment__. If you have a lot of
  these in your collection they might be updated to reflect the English
  edition, which you might not want. This is in progress but it will need some
  time and testing.

## Usage

> [!CAUTION]
> This **will** modify your library. __Please__ back up your database _and
> confirm you know how to restore it_ before experimenting this.

Navigate to `/settings/development` and update `Metadata Provider Source` with
the address of your desired metadata service. Click `Save`.

![/settings/development](./.github/config.png)

You can now search and add authors or works not available on the official
service.

If at any point you want to revert to the official service, simply delete the
`Metadata Provider Source` and save your configuration again. Any works you
added should be preserved.

> [!IMPORTANT]
> Metadata is periodically refreshed and in some cases existing files may
> become unmapped (see note above about subtitles). You can correct this from
> `Library > Unmapped Files`, or do a `Manual Import` from an author's page.

### Self-hosting

An image is available at `docker.io/blampe/rreading-glasses`. It requires a
Postgres backend, and its flags currently look like this:

```
Usage: rreading-glasses --upstream=STRING [flags]

Flags:
  -h, --help                                    Show context-sensitive help.
      --port=8788
      --rpm=60                                  maximum upstream requests per minute
      --cookie=STRING                           cookie to use for upstream HTTP requests
      --verbose                                 increase log verbosity
      --postgres-host="localhost"
      --postgres-user="postgres"
      --postgres-password=""
      --postgres-port=5432
      --postgres-database="rreading-glasses"
      --proxy=""                                HTTP proxy URL to use for upstream requests
      --upstream=STRING                         upstream host (e.g. www.example.com)
```

## Details

The open-source portion of this project implements an API-compatible,
coalescing read-through cache for consumption by the R— metadata client. It is
not a fork of any prior work.

Importantly, it does _not_ currently include any logic to actually extract
metadata -- __you will need to roll your own for now__.

### Getters

The service is pluggable and can serve metadata from any number of sources: API
clients, data dumps, OpenLibrary proxies, scrapers, or other means. The
interface to implement is:

```go
type Getter interface {
    GetWork(ctx context.Context, workID int64) (*WorkResource, error)
    GetAuthor(ctx context.Context, authorID int64) (*AuthorResource, error)
    GetBook(ctx context.Context, bookID int64) (*WorkResource, error)
}
```

In other words, anything that understands how to map an ID to a Resource can
serve as a source of truth. This project then provides caching and API routes
for that source.

In the future I might bundle some scraping logic to make self-hosting (with
your own credentials) easier, but scraping is not yet implemented because I'm
doing something very novel (no pun intended).

## Contributing

This is primarily a personal project that fixes my own workflows. There are
almost certainly edge cases I haven't accounted for, so contributions are very
welcome!

### TODO

- [ ] (Prod) Publish Docker image.
- [ ] (Prod) Add Cloudflare client for CDN invalidation.
- [ ] (Prod) Better translation support.
- [ ] (QOL) Ignore works/editions without publisher to cut down on
      self-published ebook slop.
- [ ] (QOL) Update R— client to send `Accept-Encoding: gzip` headers.

## Disclaimer

This software is provided "as is", without warranty of any kind, express or
implied, including but not limited to the warranties of merchantability,
fitness for a particular purpose and noninfringement.

In no event shall the authors or copyright holders be liable for any claim,
damages or other liability, whether in an action of contract, tort or
otherwise, arising from, out of or in connection with the software or the use
or other dealings in the software.

This software is intended for educational and informational purposes only. It
is not intended to, and does not, constitute legal, financial, or professional
advice of any kind. The user of this software assumes all responsibility for
its use or misuse.

The user is free to use, modify, and distribute the software for any purpose,
subject to the above disclaimers and conditions.
