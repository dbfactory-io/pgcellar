# The pgCellar manual

The manual is Markdown, one file per chapter, in English. `index.md` gives the title, the introduction and the order of the chapters:

```
1. [Getting started](01-getting-started.md) - one-line description.
```

The generator reads that list. The file name without its number is the page address: `01-getting-started.md` becomes `/docs/getting-started/`.

## Regenerate

From the repository root:

```
make docs        # HTML pages in site/docs/, and site/sitemap.xml
make docs-pdf    # the same manual as site/docs/pgcellar-manual.pdf (runs make docs first)
```

`make docs-pdf` prints `site/docs/_manual.html` (one page, with a title page, contents and numbered chapters) with headless Chrome or Chromium. It looks for Google Chrome on macOS, then `google-chrome`, `chromium` and similar names on Linux. Without any of them it says so and stops, and the HTML manual is still built.

The generator is a separate Go module, `tools/docs/` (it uses goldmark), so the product gets no extra dependency. The root `go test ./...` and `go vet ./...` do not see it. To run it by hand:

```
cd tools/docs && go run . -root ../..
```

The version in the page header is the latest git tag, or the newest version heading of `CHANGELOG.md` when there is no tag. Pass `-version X.Y.Z` to force one.

## Screenshots

Chapters refer to images as `images/<name>.png`:

| File | Shows |
|---|---|
| `status.png` | The Status page |
| `server-overview.png` | A server page, overview |
| `timeline.png` | The Timeline section |
| `setup.png` | The Server setup section |
| `drills.png` | The Restore drills section |
| `restore-run.png` | A restore, from the Restore page to its result |
| `runs.png` | The Runs page |
| `login.png` | The sign-in page |
| `alerts.png` | The Alerts page |

When a file is missing, `make docs` writes a grey placeholder so the build passes. Replace the placeholder with the real screenshot (about 1280 px wide, PNG) and run `make docs` again; the generator never overwrites an image that exists.

## Deploy

`site/` is static. Copy it to the web server as it is. `site/docs/_manual.html` is only the source of the PDF: it is ignored by git and does not need to be published (`rsync --exclude '_manual.html'`).

## Writing rules

- Short sentences, concrete examples, exact commands. No marketing.
- Use only straight quotes and plain hyphens in text.
- Link to another chapter with its file name: `[Alerts](08-alerts.md)`. The generator turns it into the right address, in the pages and in the PDF.
- Keep a heading unique inside its chapter: the sections get their anchors from their titles.
