# Documentation style

This guide is for anyone writing `README.md` or a page in `docs/`, whether you're a person or an agent. The pages
are published at <https://matchory.github.io/tslink/> and read on GitHub, so they have to work in both places.

## Voice

Write the way an experienced colleague explains something at a whiteboard: directly, precisely, and without padding.

- **Talk to the reader.** Use "you" and the present tense: "You give each container a hostname", not "The user
  configures a hostname". Contractions are fine.
- **Lead with what the reader wants.** Open a page or section with the outcome or the answer, then the details.
  Nobody needs "This page describes…" first.
- **Give the reason with the fact.** "Put `100.100.100.100` first: Docker asks only the first server and takes its
  answer" is easier to follow and to remember than the rule alone.
- **Be exact.** Name the command, value, default and condition. When something depends on a condition, state the
  condition instead of writing "usually" or "in most cases". When you quote a number, say where it was measured.
- **Be honest about limits.** Say plainly what tslink doesn't do, what can go wrong, and whom the reader has to
  trust. A reader who is told the limits trusts the rest of the page.
- **Don't sell.** No "seamless", "robust", "powerful", "effortless" or "simply". If a feature is good, the facts
  will show it.

### Tells to avoid

Prose that reads generated loses the reader's trust. Cut these on sight:

- An opener that restates the heading ("In this guide, we will look at…").
- A closing paragraph that summarizes what the reader just read.
- Lists of three adjectives or three parallel clauses where one would do.
- Bold labels on every bullet when the bullets are short sentences anyway.
- "Note that", "It's worth mentioning", "Importantly".
- Linking words that only connect: "Thus", "Hence", "Additionally", "Furthermore".
- Hedges that stand in for a missing fact: "may", "might", "typically", "generally".

## Page types

Each page has one job. The type goes in the page's frontmatter as `category`.

| Type | `category` | Where | Its job |
| --- | --- | --- | --- |
| Tutorial | `tutorial` | `docs/getting-started.md` | Take a new user from nothing to a working result, one example, start to finish |
| Guide | `guide` | `docs/guides/` | Help an operator do one task, including how to tell it worked |
| Reference | `reference` | `docs/reference/` | List every fact about one interface, so the reader can look it up |
| Explanation | `explanation` | `docs/explanation/` | Say why tslink works the way it does, and what follows from that |

A guide usually starts with what you'll have at the end and what you need first, then the steps, then how to
check the result and where to go next. Skip a section when it would be empty.

A reference page has a short introduction, then one entry per item (a label, option or setting): its heading, a
table of facts (value, default), then what it does and its conditions, in prose or a short list. Reference pages
don't give advice; they link to the guide that does.

Every fact has one home. Other pages link to it instead of repeating it, unless the reader needs it right there to
follow a step.

## Frontmatter

Every page that's published on the site starts with frontmatter:

```yaml
---
title: Expose a service
description: Serve a container over HTTPS in your tailnet, or make it a backend of a Tailscale Service.
category: guide
tags: [serve, services, https]
---
```

`description` is one sentence. It appears in search results and link previews.

## Terms

Use one term for one concept.

| Use | Meaning | Not |
| --- | --- | --- |
| container | A Docker container on a tslink network | workload; task, except for behavior specific to Swarm |
| node | A device in the tailnet | machine; device, except when quoting the admin console |
| host | The computer that runs Docker and the plugin | Docker node; say *Swarm node* when Swarm matters |
| auth key | A `tskey-auth-…` key | key (alone), token |
| OAuth client secret | A `tskey-client-…` secret | client key |
| credential | An auth key or an OAuth client secret | secret (alone) |
| cluster credential | The OAuth client secret in the file `oauth-client.secret` | shared secret |
| Service | A Tailscale Service (`svc:<name>`) | VIP service; *service* in lowercase, except for a Swarm service |
| hostname | The name of a node in the tailnet, set by `tslink.hostname` | `host name` |
| data directory | `/var/lib/docker-plugins/tailscale` on the host | state dir, plugin dir |
| state directory | The directory of one node's Tailscale state in the data directory | state dir |
| endpoint | The Docker endpoint of a container; use it only in reference pages | — |

## Markdown

- One H1 per file, matching the frontmatter `title`.
- Lines of at most 120 characters. Tables are exempt.
- `-` for bullets; number ordered lists `1.`, `2.`, `3.`.
- Give every fenced code block a language: `bash`, `yaml`, `json`, `ini`, `text` or `mermaid`.
- Link between pages with relative `.md` links, anchors included:
  `[health](../reference/container-labels.md#tslinkhealth)`. They work on GitHub, and the site rewrites them.
- Draw diagrams in Mermaid. The site renders them; GitHub does too.
- Use `> [!WARNING]` only for a risk to data, security or availability, and `> [!NOTE]` for anything else worth
  pulling out. Use them sparingly: a page full of callouts has none.
- Put commands, options, labels, paths and values in code format.
- Don't write dates or provisional text such as "still to be tested". History belongs in `CHANGELOG.md`.

`SECURITY.md` holds the security guarantees and the environment properties they assume. Tests read its `### G<n>:`
headings and `Manual:` lines, so don't change their format.

## Before and after

| Before | After |
| --- | --- |
| This page tells how tslink gets HTTPS certificates for served names, and how to stay within the limits of Let's Encrypt. | When a container serves HTTPS, Tailscale gets a Let's Encrypt certificate for its name. Here's how that works, and how to stay within Let's Encrypt's rate limits. |
| Docker sends each query to the first server and accepts its answer. Thus `100.100.100.100` must be first. | Put `100.100.100.100` first: Docker sends each query to the first server only, and takes whatever it answers. |
| Docker gives the plugin no way to identify a container while it starts. Thus tslink starts Tailscale after the container starts. | While a container starts, Docker doesn't tell the plugin which container it is. So tslink waits for the container to start, then brings up its node. |
| Find the symptom below, then do the checks in the sequence of the table. | Find your symptom below and work through its checks from the top; each one rules out a cause before the next. |
| A container keeps the identity of the earlier container with the same hostname. | A new container with the same hostname picks up where the old one left off: same node, same tailnet address. |
| For most use cases, use an ephemeral, reusable, pre-approved key. | Use an auth key that's ephemeral, reusable and pre-approved: tslink removes the node when its container goes away, and every container on the network can use the same key. |
