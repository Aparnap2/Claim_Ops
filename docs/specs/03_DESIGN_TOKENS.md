# ClaimOps — Design Tokens

## Status
Backend-first project. These tokens are a future UI contract, not a reason to introduce a frontend prematurely.

## Typography
Use a neutral system sans-serif stack unless a product UI decision establishes another approved font.

Scale:
- xs: 12px
- sm: 14px
- md: 16px
- lg: 18px
- xl: 24px
- 2xl: 30px
- 3xl: 36px

## Spacing
Base unit: 4px.

- 1: 4px
- 2: 8px
- 3: 12px
- 4: 16px
- 5: 20px
- 6: 24px
- 8: 32px
- 10: 40px
- 12: 48px

## Radius
- sm: 4px
- md: 8px
- lg: 12px
- xl: 16px
- pill: 999px

## Semantic colors
Use semantic names rather than hard-coded colors in components.

- text.primary
- text.secondary
- surface.default
- surface.muted
- border.default
- status.info
- status.success
- status.warning
- status.error
- status.neutral
- action.primary
- action.danger

Exact color values should be defined only when a real UI implementation is introduced and accessibility contrast has been checked.

## Data visualization
- Do not use color as the sole encoding.
- Prefer labels/icons plus color.
- Avoid decorative charts for operational decisions.
- Confidence/uncertainty must never be represented as an arbitrary color gradient without an explicit semantic scale.

## Layout
- Dense information is acceptable for an operations console.
- Preserve clear hierarchy.
- Evidence and source context should remain visually adjacent to the fact it supports.
