import packageJson from "../../package.json" with { type: "json" };

export const repoUrl: string = packageJson.repository;

export interface NavItem {
  slug: string;
  title: string;

  /**
   * Force full page reload when navigating to this page (skips View Transitions).
   */
  reload?: boolean;
}

export interface NavGroup {
  label: string;
  items: NavItem[];
}

const allPages: NavItem[] = [];

export function getPrevNext(slug: string): { prev: NavItem | null; next: NavItem | null } {
  if (!allPages.length) {
    for (const { items } of sidebarGroups) {
      allPages.push(...items);
    }
  }

  const index = allPages.findIndex((item) => item.slug === slug);

  return {
    prev: index > 0 ? allPages[index - 1] : null,
    next: index >= 0 && index < allPages.length - 1 ? allPages[index + 1] : null,
  };
}

export const sidebarGroups: NavGroup[] = [
  {
    label: "Start here",
    items: [{ slug: "getting-started", title: "Getting started" }],
  },
  {
    label: "Guides",
    items: [
      { slug: "guides/install-and-upgrade", title: "Install and upgrade" },
      { slug: "guides/provision-a-node", title: "Provision a host" },
      { slug: "guides/deploy-on-swarm", title: "Deploy on Swarm" },
      { slug: "guides/expose-a-service", title: "Expose a service" },
      { slug: "guides/zero-downtime-updates", title: "Update without downtime" },
      { slug: "guides/monitor", title: "Monitor tslink" },
      { slug: "guides/troubleshoot", title: "Troubleshoot" },
    ],
  },
  {
    label: "Reference",
    items: [
      { slug: "reference/network-options", title: "Network options" },
      { slug: "reference/container-labels", title: "Container labels" },
      { slug: "reference/plugin-settings", title: "Plugin settings" },
      { slug: "reference/credentials", title: "Credentials" },
      { slug: "reference/readiness-endpoint", title: "Readiness endpoint" },
      { slug: "reference/files-and-paths", title: "Files and paths" },
      { slug: "reference/diag", title: "tslink diag" },
    ],
  },
  {
    label: "Explanation",
    items: [
      { slug: "explanation/architecture", title: "Architecture" },
      { slug: "explanation/security-model", title: "Security model" },
      { slug: "explanation/https-certificates", title: "HTTPS certificates" },
      { slug: "explanation/limitations", title: "Limitations" },
    ],
  },
];
