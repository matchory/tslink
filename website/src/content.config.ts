import { glob } from "astro/loaders";
import { z } from "astro/zod";
import { defineCollection } from "astro:content";

/**
 * The pages the site publishes, named folder by folder rather than as
 * `**\/*.md`: `docs/` also holds the style guide, the contributor pages and the
 * design specs, which stay on GitHub, and on a maintainer's machine untracked
 * notes that must never reach the public site.
 */
export const docsPattern = [
  "getting-started.md",
  "guides/*.md",
  "reference/*.md",
  "explanation/*.md",
];

const docs = defineCollection({
  loader: glob({ pattern: docsPattern, base: "../docs" }),
  schema: z.object({
    title: z.string(),
    description: z.string(),
    category: z.enum(["tutorial", "guide", "reference", "explanation"]),
    tags: z.array(z.string()),
  }),
});

export const collections = { docs };
