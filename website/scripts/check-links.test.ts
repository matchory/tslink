import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { checkDist, checkSource } from "./check-links.ts";

function tree(files: Record<string, string>): string {
  const root = mkdtempSync(join(tmpdir(), "check-links-"));

  for (const [path, content] of Object.entries(files)) {
    mkdirSync(dirname(join(root, path)), { recursive: true });
    writeFileSync(join(root, path), content);
  }

  return root;
}

test("a link under the base to a built page passes", () => {
  const dist = tree({
    "index.html": '<a href="/tslink/guides/x">x</a>',
    "guides/x.html": "<h1>x</h1>",
  });

  assert.deepEqual(checkDist(dist, "/tslink"), []);
});

test("a root-relative link that leaves the base fails, naming file and link", () => {
  const dist = tree({
    "index.html": '<a href="/guides/x">x</a>',
    "guides/x.html": "<h1>x</h1>",
  });

  assert.deepEqual(checkDist(dist, "/tslink"), ["index.html: /guides/x is outside /tslink"]);
});

test("a link under the base to a page that was not built fails", () => {
  const dist = tree({ "index.html": '<img src="/tslink/missing.svg">' });

  assert.deepEqual(checkDist(dist, "/tslink"), ["index.html: /tslink/missing.svg does not exist"]);
});

test("a link to an anchor the built page lacks fails", () => {
  const dist = tree({
    "index.html": '<a href="/tslink/guides/x#nope">x</a>',
    "guides/x.html": '<h2 id="yes">yes</h2>',
  });

  assert.deepEqual(checkDist(dist, "/tslink"), [
    "index.html: /tslink/guides/x#nope has no such anchor",
  ]);
});

test("a relative link in a built Markdown page that leaves the base fails", () => {
  const dist = tree({
    "explanation/x.md": "See [ok](../guides/y.md) and [security](../../SECURITY.md).\n",
    "guides/y.md": "# y\n",
  });

  assert.deepEqual(checkDist(dist, "/tslink"), [
    "explanation/x.md: ../../SECURITY.md is outside /tslink",
  ]);
});

test("a source link to a missing file fails", () => {
  const root = tree({ "docs/guides/a.md": "See [it](../reference/missing.md).\n" });

  assert.deepEqual(checkSource(root, ["docs/guides/a.md"]), [
    "docs/guides/a.md: ../reference/missing.md does not exist",
  ]);
});

test("a source link to a heading that does not exist fails, by GitHub's slug rules", () => {
  const root = tree({
    "docs/guides/a.md":
      "See [ok](../reference/b.md#tslink_log_level) and [bad](../reference/b.md#nope).\n",
    "docs/reference/b.md": "# B\n\n## TSLINK_LOG_LEVEL\n",
  });

  assert.deepEqual(checkSource(root, ["docs/guides/a.md"]), [
    "docs/guides/a.md: ../reference/b.md#nope has no such heading",
  ]);
});

test("a heading's code span counts toward its slug, as on GitHub", () => {
  const root = tree({
    "docs/a.md": "See [it](b.md#docker-network-create-fails).\n",
    "docs/b.md": "# B\n\n## `docker network create` fails\n\ntext\n",
  });

  assert.deepEqual(checkSource(root, ["docs/a.md"]), []);
});

test("links inside code are not links", () => {
  const root = tree({
    "docs/a.md": "Write `[x](missing.md)`, or:\n\n```markdown\n[y](missing.md)\n```\n",
  });

  assert.deepEqual(checkSource(root, ["docs/a.md"]), []);
});
