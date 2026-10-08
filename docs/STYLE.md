# Documentation style

These rules apply to `README.md` and to all pages in `docs/`. They are adapted from
[ASD-STE100 Simplified Technical English](https://www.asd-ste100.org/): we use its writing rules, but not its
dictionary.

## Document types

Each page has one type. Do not mix the types on one page.

| Type | Folder | Purpose | Form |
| --- | --- | --- | --- |
| Tutorial | `docs/getting-started.md` | Teach a new user with one complete example | Numbered sections that build on each other |
| Guide | `docs/guides/` | Help an operator do one task | The guide structure below |
| Troubleshooting | `docs/guides/troubleshoot.md` | Help an operator find the cause of a symptom | One section for each symptom, with a table of causes |
| Reference | `docs/reference/` | Give complete facts about one interface | A short introduction, then tables |
| Explanation | `docs/explanation/` | Tell why tslink works as it does | Prose, with measurements and their conditions |

## Sentences

- Write procedural sentences of 20 words or fewer. Write descriptive sentences of 25 words or fewer.
- Write one idea in each sentence. Write one instruction in each step.
- Put a condition before the instruction: "If the MTU is below 1500, set the MTU in `daemon.json`."
- Use the active voice. Use the imperative in procedures. Use the present tense.
- Do not use an -ing form as a noun or as a modifier without a clear subject.
- Do not use contractions.
- Write six sentences or fewer in a paragraph. If you have more than three parallel items, use a list.

## Words

- Use one term for one concept. Use the terms in [Terms](#terms), and no synonyms.
- Use "must" for a requirement, "can" for a capability and "do not" for a prohibition.
- Do not hedge. State the condition, or give the measurement and where it comes from.
- Do not use these words outside code:

  | Word | Use instead |
  | --- | --- |
  | should | must, or state the condition |
  | may | can, or state the condition |
  | usually, typically | state the condition |
  | simply, just, easily, basically, obviously | delete the word |
  | note that | delete the words, or use a note |
  | approximately, about (with a number) | give the measured value and its conditions |

- Technical names such as tailscaled, Swarm and Serve are permitted. Put commands, options, labels, paths and values
  in code format.

## Structure

A guide has these sections, in this sequence:

```markdown
## Goal
One sentence: what the reader has at the end.

## Prerequisites
What must be true before step 1.

## Steps
1. One instruction, with its code block.

## Verify
A command and its expected output.

## Next steps
Links to the pages that follow.
```

More rules:

- A reference page has a short introduction of three sentences or fewer, then tables. It gives no advice.
- Each fact has one home. Other pages link to it and do not repeat it.
- Measurements, such as durations and throughput, go only in explanation pages and in `docs/testing.md`. Give the
  conditions of each measurement.
- Use `> [!WARNING]` only for a risk to data, security or availability. Use `> [!NOTE]` for other notes.
- Do not write dates or provisional text, such as "still to be tested". Put history in `CHANGELOG.md`.
- The security guarantees and environment properties have their home in `SECURITY.md`. Tests read its `### G<n>:`
  headings and `Manual:` lines, so do not change their format.

## Terms

| Use | Meaning | Do not use |
| --- | --- | --- |
| container | A Docker container on a tslink network | workload; task, except for behavior specific to Swarm |
| node | A device in the tailnet | machine; device, except in a quotation from the admin console |
| host | The computer that runs Docker and the plugin | Docker node; use *Swarm node* when Swarm matters |
| auth key | A `tskey-auth-…` key | key (alone), token |
| OAuth client secret | A `tskey-client-…` secret | client key |
| credential | An auth key or an OAuth client secret | secret (alone) |
| cluster credential | The OAuth client secret in the file `oauth-client.secret` | shared secret |
| Service | A Tailscale Service (`svc:<name>`) | VIP service; *service* in lowercase, except for a Swarm service |
| hostname | The name of a node in the tailnet, set by `tslink.hostname` | `host name` |
| data directory | `/var/lib/docker-plugins/tailscale` on the host | state dir, plugin dir |
| state directory | The directory of one node's Tailscale state in the data directory | state dir |
| endpoint | The Docker endpoint of a container; use the term only in reference pages | — |

## Markdown

- Use one H1 in each file.
- Keep lines at 120 characters or fewer. Tables are exempt.
- Use `-` for bullets. Number ordered lists `1.`, `2.`, `3.`.
- Give each fenced code block a language: `bash`, `yaml`, `json`, `ini` or `text`.
- Use relative links between pages.

## Examples

| Before | After |
| --- | --- |
| The plugin will be enabled automatically. | Docker enables the plugin when the installation is complete. |
| For most use cases, use an ephemeral, reusable, pre-approved key. | Use an auth key that is ephemeral, reusable and pre-approved. |
| Without it, a container's tailnet throughput is about 40% of the host's. | Guide: "Enable UDP GRO forwarding on the uplink of the host." Explanation: "Without UDP GRO forwarding, the tailnet throughput of a container was 1.2 Gbit/s, and of the host 2.8 Gbit/s (4-vCPU VM)." |
