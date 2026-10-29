/**
 * Checks the links of the documentation in both places it is read.
 *
 * `node scripts/check-links.ts` checks the built site in `dist/`: every `href`
 * and `src` has to stay under the base path, the site is served from a project
 * path of the organization's Pages domain, and point at a file and anchor the
 * build produced.
 *
 * `node scripts/check-links.ts --source` checks the Markdown as GitHub renders
 * it: every relative link in the repository's tracked Markdown has to point at
 * a file that exists, and at a heading that exists, by GitHub's slug rules.
 */
import { execFileSync } from "node:child_process";
import { existsSync, readFileSync, readdirSync, statSync } from "node:fs";
import { dirname, join, normalize, posix, resolve } from "node:path";
import GithubSlugger from "github-slugger";

const external = /^([a-z][a-z0-9+.-]*:|\/\/)/i;

function filesEndingIn(dir: string, extension: string): string[] {
  return readdirSync(dir, { recursive: true, encoding: "utf-8" })
    .filter((path) => path.endsWith(extension))
    .sort();
}

function builtFile(dist: string, path: string): string | null {
  for (const candidate of [path, `${path}.html`, join(path, "index.html")]) {
    const file = join(dist, candidate);

    if (existsSync(file) && statSync(file).isFile()) {
      return file;
    }
  }

  return null;
}

export function checkDist(dist: string, base: string): string[] {
  const problems: string[] = [];

  for (const page of filesEndingIn(dist, ".html")) {
    const html = readFileSync(join(dist, page), "utf-8");

    for (const [, url] of html.matchAll(/\s(?:href|src)="([^"]*)"/g)) {
      if (external.test(url) || url.startsWith("#") || url === "") {
        continue;
      }

      const [path, anchor] = url.split("#");

      if (path !== base && !path.startsWith(`${base}/`)) {
        problems.push(`${page}: ${url} is outside ${base}`);
        continue;
      }

      const file = builtFile(dist, path.slice(base.length).split("?")[0] || "/");

      if (!file) {
        problems.push(`${page}: ${url} does not exist`);
      } else if (anchor && !readFileSync(file, "utf-8").includes(`id="${anchor}"`)) {
        problems.push(`${page}: ${url} has no such anchor`);
      }
    }
  }

  // The Markdown copy of each page, which llms.txt hands to agents, keeps its
  // relative links, so they resolve against the page's own URL.
  for (const page of filesEndingIn(dist, ".md")) {
    for (const url of markdownLinks(readFileSync(join(dist, page), "utf-8"))) {
      if (external.test(url) || url.startsWith("#") || url.startsWith("/")) {
        continue;
      }

      const path = posix.normalize(posix.join(base, posix.dirname(page), url.split("#")[0]));

      if (!path.startsWith(`${base}/`)) {
        problems.push(`${page}: ${url} is outside ${base}`);
      } else if (!builtFile(dist, path.slice(base.length))) {
        problems.push(`${page}: ${url} does not exist`);
      }
    }
  }

  return problems;
}

/** Markdown with code blocks and code spans blanked out: links in them are examples. */
function withoutCode(markdown: string): string {
  return markdown.replace(/^(`{3,}|~{3,})[\s\S]*?^\1/gm, "").replace(/`[^`\n]*`/g, "");
}

function markdownLinks(markdown: string): string[] {
  return [...withoutCode(markdown).matchAll(/\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*")?\s*\)/g)].map(
    ([, url]) => url,
  );
}

function headingSlugs(markdown: string): Set<string> {
  const slugger = new GithubSlugger();
  const slugs = new Set<string>();

  for (const [, heading] of withoutCode(markdown).matchAll(/^#{1,6}\s+(.+?)\s*#*$/gm)) {
    slugs.add(slugger.slug(heading));
  }

  return slugs;
}

export function checkSource(root: string, files: string[]): string[] {
  const problems: string[] = [];

  for (const file of files) {
    const markdown = readFileSync(join(root, file), "utf-8");
    const links = [
      ...markdownLinks(markdown),
      ...[...markdown.matchAll(/\s(?:href|src)="([^"]+)"/g)].map(([, url]) => url),
    ];

    for (const url of links) {
      if (external.test(url)) {
        continue;
      }

      const [path, anchor] = url.split("#");
      const target = path ? normalize(join(dirname(file), decodeURI(path))) : file;

      if (!existsSync(join(root, target))) {
        problems.push(`${file}: ${url} does not exist`);
      } else if (
        anchor &&
        target.endsWith(".md") &&
        !headingSlugs(readFileSync(join(root, target), "utf-8")).has(anchor)
      ) {
        problems.push(`${file}: ${url} has no such heading`);
      }
    }
  }

  return problems;
}

function trackedMarkdown(root: string): string[] {
  return execFileSync("git", ["ls-files", "*.md"], { cwd: root, encoding: "utf-8" })
    .split("\n")
    .filter((path) => path && !path.startsWith("docs/specs/") && !path.startsWith("specs/"));
}

if (import.meta.main) {
  const repoRoot = resolve(import.meta.dirname, "../..");
  const problems = process.argv.includes("--source")
    ? checkSource(repoRoot, trackedMarkdown(repoRoot))
    : checkDist(resolve(import.meta.dirname, "../dist"), "/tslink");

  for (const problem of problems) {
    console.error(problem);
  }

  console.log(problems.length ? `${problems.length} broken links` : "all links resolve");
  process.exitCode = problems.length ? 1 : 0;
}
