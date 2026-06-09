# Overview

## IF STUCK

If you are stuck, or i correct you more than couple of times, suggest updates to this file in order to stop us from
making same mistakes in future implementations.

## Style guide

-Never write obvious comments.Comments should explain **WHY**, not **WHAT**. // BAD - Obvious, restates code // Set
agent configuration from response err = p.config.SetAgentConfig(response.AgentCommsKey, ...)

// GOOD - Explains why // Cache the comms key locally to avoid re-registration on restart err =
p.config.SetAgentConfig(response.AgentCommsKey, ...)

-Avoid nested if else statements whenever possible

## Output

When reporting info to me be concise and sacrifice grammar for the sake of brevity.

## Editing

Prefer LSP over Grep/Read for code navigation

Use eza --tree to always be aware of project structure

## GIT

When creating git commits omit co-authored by messages
