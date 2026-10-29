import type { APIContext } from "astro";
import { siteResources, siteSections } from "@/lib/manifest";
import { generatedPage } from "@/lib/pages";

/**
 * `llms.txt`, as https://llmstxt.org describes it: an H1, a blockquote summary,
 * optional prose, then H2 sections of `- [name](url): notes` links.
 *
 * Generated from the manifest, in the sidebar's order, so it cannot fall behind
 * the navigation. Links point at the Markdown representation of each page
 * rather than the HTML: an agent following one gets the page, not a shell of
 * markup around it.
 */
export async function GET({ site }: APIContext) {
  const url = (path: string) => new URL(path, site).href;
  const sections = await siteSections();

  const lines = [
    "# tslink",
    "",
    `> ${generatedPage("/").description}`,
    "",
    "tslink is a Docker network plugin. For every container on a tslink network, it runs a tailscaled in the container's network namespace, so the container is a node of its own in a Tailscale tailnet, configured with container labels. It works on a single host and on Docker Swarm.",
    "",
    "Every documentation page on this site has a Markdown representation at the same path with `.md` appended, which is what the links below point at.",
  ];

  for (const { label, pages } of sections) {
    lines.push("", `## ${label}`, "");

    for (const { description, markdown, path, title } of pages) {
      lines.push(`- [${title}](${url(markdown ?? path)}): ${description}`);
    }
  }

  lines.push("", "## Optional", "");

  for (const { description, path, title } of siteResources) {
    // Not this file. An agent reading it has already found it.
    if (path.endsWith("/llms.txt")) {
      continue;
    }

    lines.push(`- [${title}](${url(path)}): ${description}`);
  }

  return new Response(lines.join("\n") + "\n", {
    headers: { "Content-Type": "text/plain; charset=utf-8" },
  });
}
