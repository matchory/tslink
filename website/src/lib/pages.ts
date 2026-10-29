import { existsSync } from "node:fs";
import { resolve } from "node:path";
import { getLastModifiedSync, isShallowSync } from "./git";

/**
 * A page this site serves that is not a doc in the content collection: the
 * home page.
 *
 * The title and description are the page's own copy: the `.astro` page reads
 * them from here rather than declaring them a second time, so `llms.txt`
 * cannot describe the page differently from the page itself.
 */
export interface GeneratedPage {
  path: string;
  title: string;
  description: string;

  /**
   * The Markdown representation of this page, if it has one.
   */
  markdown?: string;

  /**
   * The file whose last commit dates this page, relative to the repository
   * root.
   */
  sourcePath: string;
}

export const generatedPages: GeneratedPage[] = [
  {
    path: "/",
    title: "Home",
    description:
      "A Docker network plugin that puts every container on your tailnet as a node of its own.",
    sourcePath: "website/src/pages/index.astro",
  },
];

export function generatedPage(path: string): GeneratedPage {
  const page = generatedPages.find((candidate) => candidate.path === path);

  if (!page) {
    throw new Error(`no generated page for ${path}`);
  }

  return page;
}

/**
 * The Markdown representation of a generated page, for the pages that have one.
 */
export function markdownFor(path: string): string {
  const { markdown } = generatedPage(path);

  if (!markdown) {
    throw new Error(`${path} has no Markdown representation`);
  }

  return markdown;
}

const docsDir = resolve("../docs");

/**
 * The source file behind a doc slug, relative to the repository root. It is
 * probed rather than read from the content collection because
 * `astro.config.ts` runs before the content layer exists and still needs to
 * date every page.
 */
function docsSourcePath(slug: string): string | null {
  return existsSync(resolve(docsDir, `${slug}.md`)) ? `docs/${slug}.md` : null;
}

/**
 * The source file behind a rendered path, relative to the repository root.
 */
export function sourceForPath(pathname: string): string {
  const page = generatedPages.find((candidate) => candidate.path === pathname);

  if (page) {
    return page.sourcePath;
  }

  const source = docsSourcePath(pathname.replace(/^\//, ""));

  if (!source) {
    throw new Error(`no source file for ${pathname}; add it to generatedPages`);
  }

  return source;
}

/**
 * The `lastmod` for one sitemap entry. Throwing rather than omitting the date is
 * the point: a page added without a source mapping fails `astro build` instead
 * of shipping a sitemap entry that never appears to change. `sitemapRequired`
 * in `astro.config.ts` is what makes a throw here fail the build — the sitemap
 * integration itself catches and logs whatever `serialize` raises.
 */
export function lastModifiedFor(url: string): string {
  if (isShallowSync()) {
    throw new Error(
      "a shallow clone dates every page from its one commit; check out with fetch-depth: 0",
    );
  }

  // Sitemap URLs carry the deployment's base; the page registry does not.
  const pathname = new URL(url).pathname.replace(/^\/tslink(?=\/|$)/, "") || "/";
  const source = sourceForPath(pathname === "/" ? "/" : pathname.replace(/\/$/, ""));
  const modified = getLastModifiedSync(source);

  if (!modified) {
    throw new Error(`no commit in this clone touches ${source}; commit it before building`);
  }

  return modified.toISOString();
}
