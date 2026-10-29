import { dirname, relative, resolve } from "node:path";
import packageJson from "../../package.json" with { type: "json" };

/**
 * The site is served from `https://matchory.github.io/tslink/`, the GitHub
 * Pages domain of the repository, so every path it serves sits under this.
 * `astro.config.ts` sets the same value as Astro's `base`.
 */
export const base = "/tslink";

const repoRoot = resolve("..");
const docsRoot = resolve("../docs");

/**
 * Whether a file, relative to `docs/`, is a page the site publishes. Must agree
 * with `docsPattern` in `src/content.config.ts`, which this module cannot
 * import: it pulls in `astro:content`, and `astro.config.ts` imports this.
 */
const isPublished = (path: string) =>
  /^(getting-started|(guides|reference|explanation)\/[^/]+)\.md$/.test(path);

/**
 * The site's URL for a link written for GitHub: a relative path from the doc it
 * is in, such as `../reference/container-labels.md#tslinkhealth`. A link to a
 * published page becomes that page's path under `base`; a link to anything else
 * in the repository, such as `SECURITY.md` or the style guide, goes to the file
 * on GitHub, where it lives.
 */
export function siteUrlFor(url: string, fromFile: string): string {
  if (/^([a-z]+:|\/\/|#)/i.test(url)) {
    return url;
  }

  const [path, hash] = url.split(/(?=#)/);
  const target = resolve(dirname(fromFile), path);
  const inDocs = relative(docsRoot, target);

  if (isPublished(inDocs)) {
    return `${base}/${inDocs.replace(/\.md$/, "")}${hash ?? ""}`;
  }

  return `${packageJson.repository}/blob/main/${relative(repoRoot, target)}${hash ?? ""}`;
}

/**
 * A doc's Markdown with its links to files the site doesn't publish pointed at
 * GitHub. Links between published pages stay relative: the site serves each
 * page's Markdown next to its HTML, so they resolve as they do on GitHub.
 */
export function markdownForSite(markdown: string, fromFile: string): string {
  return markdown.replace(/(\]\(\s*<?)([^)\s>]+)/g, (match, open: string, url: string) => {
    const mapped = siteUrlFor(url, fromFile);

    return mapped.startsWith("http") && mapped !== url ? `${open}${mapped}` : match;
  });
}
