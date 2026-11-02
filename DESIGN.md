---
name: Chorus website
description: The public website inherits the Chorus dashboard's light surfaces, navy text, blue actions, and four-color logo.
colors:
  ink: "#0f172a"
  secondary: "#334155"
  blue: "#1d4ed8"
  blue-dark: "#173da8"
  line: "#cbd5e1"
  soft: "#edf3ff"
  surface: "#f8fafc"
  white: "#fff"
  brand-amber: "#F59E0B"
  brand-teal: "#14B8A6"
  brand-blue: "#3B82F6"
  brand-coral: "#F43F5E"
typography:
  display:
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif'
    fontSize: "clamp(48px, 5.3vw, 78px)"
    fontWeight: 700
    lineHeight: 1.045
    letterSpacing: "-.04em"
  headline:
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif'
    fontSize: "clamp(32px, 3.3vw, 46px)"
    fontWeight: 700
    lineHeight: 1.15
    letterSpacing: "-.035em"
  title:
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif'
    fontSize: "19px"
    fontWeight: 700
    lineHeight: 1.35
    letterSpacing: "-.015em"
  body:
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif'
    fontSize: "16px"
    fontWeight: 400
    lineHeight: 1.65
  label:
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif'
    fontSize: "13px"
    fontWeight: 400
    lineHeight: 1.65
  code:
    fontFamily: 'ui-monospace, "SFMono-Regular", Consolas, monospace'
    fontSize: "13px"
    fontWeight: 400
    lineHeight: 1.8
rounded:
  copy-control: "6px"
  outlined-link: "9px"
  action: "10px"
  command: "12px"
  example: "14px"
  dashboard: "18px"
spacing:
  compact: "12px"
  small: "16px"
  medium: "24px"
  wide: "28px"
  large: "40px"
components:
  button-primary:
    backgroundColor: "{colors.blue}"
    textColor: "{colors.white}"
    rounded: "{rounded.action}"
    padding: "16px 24px"
  button-primary-hover:
    backgroundColor: "{colors.blue-dark}"
    textColor: "{colors.white}"
  button-copy:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.secondary}"
    rounded: "{rounded.copy-control}"
    padding: "7px 10px"
  command-card:
    backgroundColor: "{colors.white}"
    textColor: "{colors.ink}"
    rounded: "{rounded.command}"
    padding: "22px 24px"
  message-example:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.blue}"
    rounded: "{rounded.example}"
    padding: "30px"
---

# Design System: Chorus website

## Overview

**Creative North Star: "Inherited Chorus dashboard"**

The website carries the existing Chorus identity into a spacious, light public page. It uses the original four-color mark, cool white surfaces, navy text, and blue actions. The real dashboard supplies the product imagery; its example-data caption stays with it.

This records the implemented static page, rather than a new identity or a complete application component library. The visual source is [site/styles.css](site/styles.css), with markup in [site/index.html](site/index.html) and copy feedback in [site/site.js](site/site.js).

**Key Characteristics:**

- Clear system sans-serif text and strongly sized headings.
- Pale blue regions, softly rounded controls, and restrained borders.
- A real dashboard screenshot and selectable installation commands.

## Colors

### Primary

Blue identifies the main installation action, text links, numbered steps, code examples, and keyboard focus. Its darker companion appears on the primary action's hover state.

### Secondary

Amber, teal, brighter blue, and coral belong to the existing logo and product imagery. Preserve the supplied SVG rather than reconstructing or recoloring its parts. These logo colors are distinct from the darker blue used for readable website actions.

### Neutral

Ink is the main text color; secondary is used for explanations and captions. White is the page and installation-card background. Surface distinguishes the message example and copy controls. Line supplies dividers and outlined controls. Soft is the pale blue background behind the screenshot and installation section.

## Typography

The system sans-serif stack serves headings, body text, navigation, and controls. Commands and the message example use the system monospace stack. Headings have tight tracking and balanced wrapping; paragraphs use a more open line height.

The frontmatter captures the display, section heading, step title, body, small label, and installation-code roles. Introductory text is larger (20px), reducing to 17px at 1000px and below. Step descriptions use 15px; navigation uses 14px at weight 550. The message example uses 14px monospace. Supporting text is generally 13px.

At 700px and below, the main heading uses `clamp(42px, 9.8vw, 64px)`. Paragraph widths depend on role: the hero introduction is 40ch, section introductions 37ch, and the detail explanation 55ch.

## Layout

The centered content width is at most 1240px, with at least 48px side margins. At 1000px and below, margins become 32px; at 700px and below, they become 20px. The dashboard stage becomes slightly wider on mobile, with 12px outer margins.

Desktop uses two columns for the hero, detail explanation, and installation section; the workflow has three equal columns. At 700px and below, each becomes one column. Workflow numbers move beside the step text. The header retains Docs and GitHub while hiding the in-page workflow link; the footer wraps. The screenshot switches to its mobile image at 600px and below.

Section spacing is generous: workflow padding is 112px above and 92px below, the detail section ends with 110px, and installation has 86px vertical padding. On mobile those values become 70px/56px, 64px, and 58px respectively. Commands wrap within their containers rather than extending the page width.

## Elevation & Depth

Most surfaces use background changes and spacing for separation. A single shadow lifts the dashboard image inside its pale blue stage: `0 14px 40px rgb(15 23 42 / 12%)`. Installation cards and the message example have no shadow.

## Shapes

Controls and surfaces use modest rounded corners, with the observed radii recorded in the frontmatter. The desktop dashboard stage has the largest corners; on mobile it uses the command-card radius. Thin borders distinguish the GitHub link and copy controls. Workflow numbers are circular (32px across).

## Components

### Buttons

The blue installation link is the primary action, with white text at weight 650. Mobile padding becomes 14px by 22px. Hover darkens the background. Copy buttons use a light surface, secondary text, and a thin line border; hover makes both text and border blue.

All interactive elements share a visible blue focus outline (3px, offset 5px). Links also change to blue on hover. Navigation and action labels retain normal sentence case.

### Cards / Containers

White installation cards sit on a pale blue section, with 16px between cards. Each holds a small heading, a copy button, and wrapping monospace text. Mobile card padding becomes 20px. The message example uses a light gray background and blue monospace text; its padding reduces to 24px on mobile.

### Navigation

The header pairs the logo and name with simple text links and an outlined GitHub link. Links have vertical padding (10px); the desktop link gap is 30px. Mobile navigation uses 13px text and an 18px gap. Footer navigation follows the same typography and wraps below the brand when needed.

### Dashboard image

The real screenshot sits inside the pale blue stage, clips to rounded corners, and links to its full-size image. The centered caption explains that the conversation is example data. Mobile uses the dedicated narrow screenshot rather than squeezing the desktop image.

### Installation feedback

Copy controls appear when JavaScript runs. A successful copy announces where to paste the command. If copying fails, the command is selected and the status explains how to copy it manually. The status has reserved vertical space and a polite live announcement. Installation text remains visible and selectable without JavaScript.

In-page links scroll smoothly; the reduced-motion preference restores ordinary scrolling. No other animation is defined.

## Do's and Don'ts

### Do:

- **Do** preserve the original four-color Chorus mark.
- **Do** use blue for actions and keep keyboard focus clearly visible.
- **Do** keep commands readable, wrapping, and selectable on mobile.
- **Do** retain the example-data caption with the dashboard image.

### Don't:

- **Don't** substitute a new visual identity for the inherited dashboard identity.
- **Don't** use the logo's brighter blue in place of the established darker action blue.
