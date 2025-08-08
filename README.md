> [!IMPORTANT]
> The original R—— project has been retired, but there are community forks
> available which continue to work by using rreading-glasses metadata:
>   * [pennydreadful/bookshelf](https://github.com/Faustvii/Readarr/pkgs/container/readarr) (G——R——, Hardcover)
>   * [Faustvii/Readarr](https://github.com/Faustvii/Readarr/pkgs/container/readarr) (G——R——)

# 🤓 rreading-glasses [![Discord](https://img.shields.io/discord/1367649771237675078?label=Discord)](https://discord.gg/Xykjv87yYs)

Corrective lenses for curmudgeonly readars in your life.

tl;dr: [follow these instructions](#usage).

This is a drop-in replacement for R——'s (now defunct) metadata service. It
works with your existing R—— installation, it's backwards-compatible with your
library, and it takes only seconds to enable or disable. You can use it
permanently, or temporarily to help you add books the R—— service doesn't have
yet. It works equally well with ebooks and audiobooks.

Unlike R——'s proprietary service, this is much faster, handles large authors,
has full coverage of G——R—— (or Hardcover!), and doesn't take months to load
new books. Hosted instances are available at `https://api.bookinfo.pro`
(G——R——) and `https://hardcover.bookinfo.pro`, but it can also be self-hosted.

```mermaid
graph LR;
    R[R——]-.->M[official metadata];
    R--> api.bookinfo.pro;

    classDef dotted stroke-dasharray:2,text-decoration:line-through;
    class M dotted;
```


As of August 2025 there are ~6100 users of the shared instance. Here's what some
of them have said so far:

> Man this is wayyyyyy better than the inhouse metadata, thank you!!

> This is fucking awesome, thank you!!!

> I just added this, and omg it fixed so many issues i've been having! Thank
> you!

> Holy shit can I just say this is so much better.

> This is fucking fantastic. Came across your link, thought hmm I bet this'll
> be crap. Reinstalled R, added your site, instantly found the 5 upcoming books
> that I couldn't. thank you!

> Already had it pull in an extra book from an author that came out in September
> that wasn't originally found!
> Will definitely be a rreading glasses evangalist! haha

> My arr instance has been switched over since yesterday, and it really has
> cleaned up that instance. I've been getting a lot of use out of it.

> it worked! thanks my man, my wife will be happy with this

> Thanks so much for putting this together again, I can't tell you how much I appreciate it!

## Usage

The easiest way to use

Navigate to `http(s)://<your instance>/settings/development`. This page isn't
shown in the UI, so you'll need to manually enter the URL.

Update `Metadata Provider Source` with `https://api.bookinfo.pro` if you'd like
to use the public instance. If you're self-hosting use your own address.

Click `Save`.

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

### Before / After

![before](./.github/before.png)

![after](./.github/after.png)

## Self-hosting

An image is available at
[`blampe/rreading-glasses`](https://hub.docker.com/r/blampe/rreading-glasses).
It requires a Postgres backend (any version).

Two docker compose example files are included as a reference:
`docker-compose-gr.yml` and `docker-compose-hardcover.yml`.

The app will use as much memory as it has available for in-memory caching, so
it's recommended to run the container with a `--memory` limit or similar.

### Hardcover Auth

When using Hardcover you must set the `hardcover-auth` parameter.

* Create an account or login to [Hardcover](https://hardcover.app).
* Click on User Icon and Settings.
* Select `Hardcover API`.
* Copy the entire token **including** `Bearer`.
* Use this as the `--hardcover-auth` flag.

Note that your API key **will expire every year on January 1**, so you'll need
to regenerate it.

#### Example Hardcover Docker Compose Snippet

> \- --hardcover-auth=Bearer Q123AbC...

### Resource Requirements

Resource requirements are minimal; a Raspberry Pi should suffice. Storage
requirements will vary depending on the size of your library, but in most cases
shouldn't exceed a few gigabytes for personal use. (The published image doesn't
require any large data dumps and will gradually grow your database as it's
queried over time.)

### Troubleshooting

When in doubt, make sure you have the latest image pulled: `docker pull
blampe/rreading-glasses:latest` or `blampe/rreading-glasses:hardcover`.

If you suspect data inconsistencies, try removing R——'s `cache.db` file and
then restart the app.

You can also try deleting your Postgres database to ensure you don't have any
bad data cached.

If these steps don't resolve the problem, please create an issue!

## Key differences

I have deviated slightly from the official service's behavior to make a couple
of, in my opinion, quality of life improvements.

- Titles no longer automatically include subtitles _unless_ it's part of a
  series, or if multiple books have the same primary title. This de-clutters
  the UI, cleans up the directory layout, and improves import matching but
  __you may need to re-import some works with long subtitles__.

- The "best" (original) edition is always preferred to make cover art more
  consistently high-quality. Additionally, books are no longer returned with
  every edition ever released, because that makes manual edition selection
  difficult to impossible. Instead, at most 20 of the top editions are included
  and de-duplicated by language and title. This may change in the future.

## Details

This project implements an API-compatible, coalescing read-through cache for
consumption by the R—— metadata client. It is not a fork of any prior work and
it was made without any help from the S—— team.

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

In other words, anything that understands how to map a G——R—— ID to a Resource
can serve as a source of truth. This project then provides caching and API
routes to make that source compatible with R——.

There are currently two sources available: [Hardcover](https://hardcover.app)
and G——R——. A summary of their differences is below.

|                   | G——R——                                                                                                                                                                   | Hardcover                                                                                                                                                              |                                                                                     |
| --                | --                                                                                                                                                                       | -------------                                                                                                                                                          | "Official" metadata                                                                 |
| Summary           | Lower quality but backward-compatible with existing R—— installations. Makes all of G——R—— available, including large authors and books not available by default in R——. | Higher quality metadata but not backward compatible, so it **requires a fresh installation.**                                                                          | Didn't support some of the most popular authors and was always half a year delayed. |
| New releases?     | Supported                                                                                                                                                                | Supported                                                                                                                                                              | Unsupported                                                                         |
| Large authors?    | Supported                                                                                                                                                                | Supported, but authors include only 20 (max) books by default for now. New books can be added by manually searching.                                                   | Unsupported                                                                         |
| Source code       | Public                                                                                                                                                                   | Public                                                                                                                                                                 | Private                                                                             |
| Stability         | Stable, nearly identical behavior to official R—— metadata.                                                                                                              | Beta, still loading data.                                                                                                                                              | Unmaintained                                                                        |
| Hosted instance   | `https://api.bookinfo.pro`                                                                                                                                               | `https://hardcover.bookinfo.pro`                                                                                                                                       |                                                                                     |
| Self-hosted image | `blampe/rreading-glasses:latest`                                                                                                                                         | `blampe/rreading-glasses:hardcover`                                                                                                                                    |                                                                                     |

Please consider [supporting](https://hardcover.app/supporter) Hardcover if you
use them as your source. It's $5/month (or $50/year) and the work they are doing to break
down the G——R—— monopoly is commendable.

Postgres is used as a backend but only as a key-value store, unlike the
official server which performs expensive joins in the request path.
Additionally large authors (and books with many editions) are populated
asynchronously. This allows the server to support arbitrarily large resources
without issue.

## Contributing

This is primarily a personal project that fixes my own workflows. There are
almost certainly edge cases I haven't accounted for, so contributions are very
welcome!

### TODO

- [ ] (QOL) Ignore works/editions without publisher to cut down on
      self-published ebook slop.
- [ ] (QOL) Update R—— client to send `Accept-Encoding: gzip` headers.

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
