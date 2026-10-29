import { readFile } from "node:fs/promises";
import type { APIContext } from "astro";
import { getDocPaths } from "../lib/docs";
import { markdownForSite } from "../lib/links";
import { markdownResponse } from "../lib/markdown";
import { withBase } from "../lib/paths";

export async function getStaticPaths() {
  const docs = await getDocPaths();

  return docs.map(({ filePath, id: slug }) => ({
    params: { slug },
    props: { filePath },
  }));
}

/**
 * Every doc as its raw Markdown. This route is what the Copy page action, the
 * View as Markdown link and the AI-app links serve. The docs are plain
 * Markdown, so the file is the page, with links to files the site doesn't
 * publish pointed at GitHub.
 */
export async function GET({ params, props, site }: APIContext) {
  let content: string;

  try {
    content = await readFile(props.filePath, "utf-8");
  } catch {
    return new Response("Not found", { status: 404 });
  }

  return markdownResponse(
    markdownForSite(content, props.filePath),
    withBase(`/${params.slug}`),
    site,
  );
}
