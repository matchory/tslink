import { getCollection } from "astro:content";
import { sidebarGroups } from "./navigation";
import { withBase } from "./paths";
import { generatedPage } from "./pages";

export interface SitePage {
  path: string;
  title: string;
  description: string;
  markdown?: string;
}

export interface SiteSection {
  label: string;
  pages: SitePage[];
}

/**
 * Every documentation page, grouped and ordered the way the sidebar presents
 * them, with the home page ahead of the rest.
 *
 * The sidebar is the order a reader is offered, so it is the order an agent is
 * offered too. A sidebar entry that is neither a doc in the collection nor a
 * page in `generatedPages` throws, so a page cannot be added to the navigation
 * and silently left out of `llms.txt`.
 */
export async function siteSections(): Promise<SiteSection[]> {
  const docs = await getCollection("docs");
  const bySlug = new Map(docs.map((doc) => [doc.id, doc]));

  const sections = sidebarGroups.map(({ items, label }) => ({
    label,
    pages: items.map(({ slug }): SitePage => {
      const doc = bySlug.get(slug);

      if (!doc) {
        return generatedPage(`/${slug}`);
      }

      return {
        path: withBase(`/${slug}`),
        title: doc.data.title,
        description: doc.data.description,
        markdown: withBase(`/${slug}.md`),
      };
    }),
  }));

  const home = generatedPage("/");

  sections[0].pages.unshift({ ...home, path: withBase(home.path) });

  return sections;
}

/**
 * One machine-readable resource this site serves.
 */
export interface SiteResource {
  path: string;
  mediaType: string;
  title: string;
  description: string;
}

/**
 * The resources that are not pages.
 */
export const siteResources: SiteResource[] = [
  {
    path: withBase("/llms.txt"),
    mediaType: "text/plain",
    title: "llms.txt",
    description: "This site's contents, as llmstxt.org describes.",
  },
  {
    path: withBase("/sitemap-index.xml"),
    mediaType: "application/xml",
    title: "Sitemap index",
    description:
      "Points at the sitemap files listing every page, each with its last-modified date.",
  },
];
