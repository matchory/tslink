/**
 * A site path with the deployment's base prepended. The site is served from
 * `https://matchory.github.io/tslink/`, so a root-relative `/guides/x` would
 * leave the project and 404 on the organization's Pages domain; every internal
 * link goes through here instead.
 */
export function withBase(path: string): string {
  const base = import.meta.env.BASE_URL.replace(/\/$/, "");

  return path === "/" ? `${base}/` : `${base}${path}`;
}
