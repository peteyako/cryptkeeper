# Third-party notices

CryptKeeper is MIT-licensed (see [LICENSE](LICENSE)). It ships these third-party files, each
under its own license:

| What | Where | License |
|---|---|---|
| IBM Plex Sans and IBM Plex Mono fonts | `web/static/fonts/*.woff2` | SIL Open Font License 1.1, full text in [web/static/fonts/OFL.txt](web/static/fonts/OFL.txt) (embedded and served with the fonts) |
| Datastar client v1.0.4 | `web/static/datastar.js` | MIT, below |
| IRS Form 8949 and Schedule D (Form 1040) | `internal/tax/irs/forms/` | US federal government works, public domain (17 U.S.C. § 105) |

Go dependencies are not vendored: each module in `go.mod` carries its own license. Everything
linked into the binary is under MIT, ISC, BSD-3-Clause or Apache-2.0. The dev tool air
(`go tool air`) is GPL-3.0 and is not part of the binary.

## Datastar

    Copyright © Star Federation

    Permission is hereby granted, free of charge, to any person obtaining a copy of this software and associated documentation files (the "Software"), to deal in the Software without restriction, including without limitation the rights to use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of the Software, and to permit persons to whom the Software is furnished to do so, subject to the following conditions:

    The above copyright notice and this permission notice shall be included in all copies or substantial portions of the Software.

    THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
