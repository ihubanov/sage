# (S)AGE Memory for AI — Store Listing

## Name
SAGE Memory for AI

## Short Description (132 chars max for Chrome)
Give your AI a secure, permanent memory. Works with ChatGPT, and more — even on free plans.

## Full Description

Every time you close an AI conversation, it forgets everything. (S)AGE fixes that — securely.

This extension connects your AI's web interface to (S)AGE, an open-source memory system that runs entirely on your computer. Your AI remembers your projects, preferences, and lessons learned across every conversation.

- AES-256 encrypted, Ed25519 signed — your data never leaves your machine
- Works on free plans — no paid tier required
- 10 memory tools: remember, recall, reflect, and more
- Sidebar panel with memory stats and quick actions
- Open source, Apache 2.0 licensed

Currently supports ChatGPT (chat.openai.com). More providers coming soon.
Requires (S)AGE running locally. Download free at https://l33tdawg.github.io/sage/

## Category
Productivity

## Tags/Keywords
AI, memory, ChatGPT, persistent memory, AI assistant, privacy, local-first, encryption, open source

## Privacy Policy

(S)AGE Memory for AI does not collect, transmit, or store any user data on external servers.

All data processing occurs locally:
- Memory data is stored in a local SQLite database on your computer (~/.sage/)
- The extension communicates only with your local (S)AGE server (default: localhost:8080)
- No analytics, telemetry, or tracking of any kind
- No user accounts required
- Ed25519 keypair is generated and stored locally in the browser

The extension requests the following permissions:
- storage: To save your (S)AGE server URL and cryptographic keys locally
- Host permissions for localhost:8080: To communicate with your local (S)AGE server
- Host permissions for chatgpt.com/chat.openai.com: To inject the content script

Source code: https://github.com/l33tdawg/sage/tree/main/extension/chrome
License: Apache 2.0

## Screenshots Needed
1. (S)AGE sidebar open in ChatGPT showing connection status and memory stats
2. Quick action buttons (Initialize, Turn, Recall, Status)
3. Tool call log showing executed (S)AGE calls
4. Popup with connection status and server config
5. ChatGPT conversation using (S)AGE memory tools
