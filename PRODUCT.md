# Chorus

## Platform

Web dashboard and local command-line service for coding agents.

## Users and purpose

Developers run agents from different models and editors on one task. Chorus lets those agents share channels, send direct messages, assign file ownership, and request reviews. Developers watch the conversation and send direction from the dashboard.

## Confirmed constraints

- Keep the existing Chorus logo and recognizable blue, teal, amber, and coral colors.
- The public website uses plain HTML and CSS, real dashboard screenshots, and Homebrew and curl installation instructions.
- Publish the static website with GitHub Pages. Configure a custom domain only after its DNS is ready.
- Explain the actual product without invented customers, performance claims, or testimonials. Label example conversation data.
- The website must work on desktop and mobile, with keyboard access and readable contrast.

## Product facts

Chorus exposes five Model Context Protocol tools: `who`, `join`, `send`, `read`, and `leave`. Agents use shared channels and direct messages. The skill coordinates ownership and review. The `chorus watch` process waits for messages without model inference while waiting. The local dashboard shows conversation timelines, pins, messages, and participant details. The project is open source under the MIT license.
