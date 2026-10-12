# Business messaging and templates

Last checked: 2026-10-11.

Official message templates are possible through the WhatsApp Business Platform Cloud API. This app currently sends ordinary messages through its QR-linked account; it has no Cloud API adapter, template editor, or template-management API.

There are two different features to consider:

| Feature | How it works | Project status |
| --- | --- | --- |
| Local message preset | Save reusable text, fill variables, preview it, and authorize an ordinary send through the linked account. | Proposed in the [roadmap](roadmap.md). |
| Official business template | Send a Meta-approved template with its locale and component parameters through the Business Platform API. | Reference only; no adapter in this app. |

A local preset does not become an approved business template. Linking a WhatsApp Business app with a QR code does not create Cloud API credentials or qualify a business for API template messaging.

## What works today

The current outbound tools send text, attachments, and existing voice recordings to authorized direct chats and groups. The owner must grant a connection sending permission separately from its read access. A permitted tool call sends directly; there is no per-message confirmation queue in the app. Obtain the user's authorization before calling it.

See [outbound messaging](clients/outbound-messaging.md) for permissions, payload limits, and durable retry behavior. These tools do not create, approve, list, or send official Business Platform templates.

## Official Business Platform templates

My WhatsApp has no WhatsApp Business Platform integration. If you need Meta-approved templates, use the Business Platform separately and follow [Meta's setup guide](https://www.postman.com/meta/whatsapp-business-platform/collection/wlk6lh4/whatsapp-cloud-api) and [messaging policy](https://whatsappbusiness.com/policy/). Business Platform credentials are separate from this app's QR-linked session.

## Can the same number keep using the Business app?

Meta has announced that businesses can use the WhatsApp Business app and Business Platform together. See the [official coexistence announcement](https://about.fb.com/news/2025/09/bringing-new-tools-to-help-businesses-boost-engagement-customer-support-and-discoverability/).

Eligibility and onboarding details need investigation for a supported integration. Do not assume every account, country, or number qualifies, or that linking this app establishes coexistence. Cloud API credentials remain separate from the QR-linked session. These references do not establish Cloud API group support or access to a user's complete personal message history.

For this project's supported workflow, use the existing [QR setup](setup.md) and [outbound permissions](clients/outbound-messaging.md).
