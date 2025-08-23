# Comparison of Forks (2025-08-22)

## Matching

### Methodology

- Boot each image with a completely new DB volume.
- Add a new media root with the default Spoken/eBook quality and None metadata
  profile.[^1]
- Wait.
- Keep waiting for post-import tasks, because they usually trigger some re-matching.
- Collect stats based on the footer of the Library tab. [^2]

Baseline numbers were taken for a pre-existing library with a mix of ebooks and
audio. These numbers reflect manual fixes I've had to make to matches over time, and
they approximate the expected state of the library if matching was perfect.
These numbers do not include "wanted" authors or works.

### Images used

- ghcr.io/linuxserver/readarr@sha256:4f291128d1a5538be73c2a30473d7cdd0820e223dc8b8533b0cbc5659000dcbe
- ghcr.io/pennydreadful/bookshelf@sha256:9efcc2eff4b0a6a2a3b74c445292c1bca8bd15ed9f2cd3963f08529d2d876d9b
- ghcr.io/pennydreadful/bookshelf@sha256:7a18020983ed73905d50aaec9e1ceb4c7471ed2e08a15661100af726ac906367
- ghcr.io/faustvii/readarr@sha256:d2323cac3ea4bedbd30c4946d506c8a60b1049629e2a63d5a25bb900135be01c
- chaptarr@sha256:474be3524742a530c64612078252a753bb38861b9b697e376a5f42c82812ec42

Plus a custom build of my own with some minor tweaks on top of bookshelf.

[^1]: Chaptarr seems to no longer support a metadata profile of None.
[^2]: Chaptarr seems to report incorrect stats.

### Audio results

| Image      | Metadata Source | Matched authors  | Matched Works   | Matched Files      | Matched Disk (GB)  | Imported authors with no works on disk [^3] | Notes                                                                                                                                                                                                        |
| ---------- | --------------- | ---------------- | --------------- | ------------------ | ------------------ | --------------------------------------      | ----                                                                                                                                                                                                         |
| baseline   | RG GR           | 344              | 375             | 3883               | 167.3              | N/A                                         | Baseline.                                                                                                                                                                                                    |
| readarr    | RG GR           | 233 (68%)        | 276 (74%)       | 2495 (64.3%)       | 120.2 (71.8%)      | 0                                           |                                                                                                                                                                                                              |
| bookshelf  | RG GR           | 305 (89%)        | 354 (94%)       | 3487 (90.0%)       | 155.6 (93.0%)      | 0                                           | Status page errors.                                                                                                                                                                                          |
| bookshelf  | RG HC           | 300 (87%)        | 347 (93%)       | 3453 (88.9%)       | 155.1 (92.7%)      | 6                                           | Status page errors.                                                                                                                                                                                          |
| chaptarr   | chaptarr        | 240 (70%)        | 255 (68%) [^4]  | 2444 (62.9%) [^5]  | 114.0 (68.1%)      | 29                                          | Books tab doesn't load. Five pages of random books showed up in the wanted tab. Logs are full of errors and binary output, including control characters which kept triggering my terminal's "Print" dialog.  |
| faustvii   | RG GR           | 276 (80%)        | 323 (86%)       | 2828 (72.8%)       | 140.5 (84.0%)      | 0                                           | Nice infinite scroll for books.                                                                                                                                                                              |
| blampe     | RG GR           | 310 (90%)        | 359 (96%)       | 3364 (86.6%)       | 157.2 (94.0%)      | 0                                           | Tweaks bookshelf to match on full title + subtitle.                                                                                                                                                          |
| blampe     | RG HC           | 299 (87%)        | 347 (93%)       | 3369 (86.8%)       | 154.7 (92.5%)      | 0                                           | Tweaks bookshelf to match on full title + subtitle.                                                                                                                                                          |

### Ebook results

| Image         | Metadata Source | Matched authors  | Matched Works   | Matched Files      | Matched Disk (GB)  | Imported authors with no works on disk [^3] | Notes                                                                                                                |
| ----------    | --------------- | ---------------- | --------------- | ------------------ | ------------------ | --------------------------------------      | ----                                                                                                                 |
| baseline      | RG GR           | 235              | 313             | 314                | 1.2                | N/A                                         | Baseline.                                                                                                            |
| readarr       | RG GR           | 214 (91%)        | 282 (90%)       | 282 (89.9%)        | 1.1 (92%)          | 0                                           |                                                                                                                      |
| bookshelf     | RG GR           | 244 (104%)       | 328 (105%)      | 330 (105%) [^6]    | 1.2 (100%)         | 0                                           | Cleaned up mappings I hadn't manually fixed yet!                                                                     |
| bookshelf     | RG HC           | 212 (90%)        | 268 (86%)       | 276 (87.9%)        | 0.82 (68%)         | 0                                           | I suspect #301 is hurting this slightly.                                                                             |
| chaptarr      | chaptarr        | 191 (81%)        | ~~25483~~ [^7]  | 250 (79.6%)        | 0.94 (78%)         | 24                                          | Seems to no longer supports the "None" profile behavior, which probably contributes to the crazy import number.      |
| faustvii      | RG GR           | 169 (72%)        | 211 (67%)       | 211 (67.2%)        | 0.93 (78%)         | 0                                           | Surprised to see this do so poorly because the author has added changes to improve epub parsing, asin handling, etc. |
| blampe custom | RG GR           | 244 (104%)       | 328 (105%)      | 330 (105%)         | 1.2 (100%)         | 0                                           | Tweaks bookshelf to match on full title + subtitle.                                                                  |
| blampe custom | RG HC           | 212 (90%)        | 268 (86%)       | 278 (89%)          | 0.82 (68%)         | 0                                           | Tweaks bookshelf to match on full title + subtitle.                                                                  |

[^3]: I'm not sure what causes this, but it's not unique to Chaptarr.
[^4]: Chaptarr showed 125, 555, and 0 works imported. The books tab also didn't
      load, so I had to manually count this.
[^5]: The library tab showed 0 files imported and another tab showed 185 for
      some reason. The unmapped tab showed 1492, so this is estimated as 3883 - 1492.
[^6]: This was able to match some things that had been unmapped on my baseline
      instance. I spot checked the results and they were all reasonable, but there
      are also almost certainly some false positives in there.
[^7]: Obviously wrong. No idea what the real number is.

## Searching

I tested some search behavior while I was waiting and took note of where the
author appeared in results, how many works also appeared, and whether the works
could be added.

- "agatha christie" to test a popular author with a large number of works.
- "john gardner" to test distinct authors with the same name. (One is the
  author of the Bond series and the other of Grendel.)

| Image          | Metadata Source | "agatha christie"                                                           | "john gardner"                                                                                                                                   |
| -------------- | --------------- | --------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------    |
| readarr        | RG GR           | Position #3 with 2 books of hers.                                           | Bond & Grendel works are both shown correctly, but only the Bond author is shown (in position #1). Adding books adds the correct author.         |
| bookshelf      | RG GR           | Position #1 with 4 books of hers.                                           | Only the Grendel author is shown in position #3. Bond & Grendel books are shown. Adding books adds the correct corresponding author.             |
| bookshelf      | RG HC           | Position #1 with 15 books of hers.                                          | Bond author is shown in position #5 with a poor quality photo. Bond and Grendel books shown below the cut. Adding books adds the correct author. |
| chaptarr       | chaptarr        | Position #9 with 0 books. ~4 series shown at the very bottom.               | Both authors shown in positions #1 and #2. Bond books are shown but Grendel book is not. Attempting to add a book does nothing.                  |
| faustvii       | RG GR           | Same results as GR bookshelf except links aren't show for books.            | Same as GR bookshelf.                                                                                                                            |
