# Business messaging and templates

Last checked: 2026-10-11.

Official message templates are possible through the WhatsApp Business Platform Cloud API. This app currently sends ordinary messages through its QR-linked account; it has no Cloud API adapter, template editor, or template-management API.

There are two different features to consider:

| Feature | How it works | Project status |
| --- | --- | --- |
| Local message preset | Save reusable text, fill variables, preview it, and authorize an ordinary send through the linked account. | Proposed in the [roadmap](roadmap.md#3-reusable-local-message-presets). |
| Official business template | Send a Meta-approved template with its locale and component parameters through the Business Platform API. | Reference only; no adapter in this app. |

A local preset does not become an approved business template. Linking a WhatsApp Business app with a QR code does not create Cloud API credentials or qualify a business for API template messaging.

## What works today

The current outbound tools send text, attachments, and existing voice recordings to authorized direct chats and groups. The owner must grant a connection sending permission separately from its read access. A permitted tool call sends directly; there is no per-message confirmation queue in the app. Obtain the user's authorization before calling it.

See [outbound messaging](clients/outbound-messaging.md) for permissions, payload limits, and durable retry behavior. These tools do not create, approve, list, or send official Business Platform templates.

## What an official adapter would need

Meta's Cloud API setup uses a business portfolio, a WhatsApp Business Account, a registered business phone number, and an access token with the necessary permissions. A future adapter would need a separate setup flow for these identities and credentials. Store those credentials privately, outside the checkout; this project does not define Cloud API environment variables yet. Meta's [official Cloud API collection](https://www.postman.com/meta/whatsapp-business-platform/collection/wlk6lh4/whatsapp-cloud-api) documents the account and API setup.

Template sends identify an approved template, language/locale, and the parameters of its supported components. A proposed adapter should expose the template's current approval status and category, validate the selected locale and parameters, and support the permitted media and button components. See Meta's [interactive template example](https://www.postman.com/meta/whatsapp-business-platform/request/lwtlz1k/send-message-template-interactive).

Business-initiated messages require approved templates. Within the customer-service window of 24 hours after the user's last message, the platform permits replies without a template. Recipient opt-in, opt-out, and other policy requirements still apply. Verify the current [WhatsApp Business Messaging Policy](https://whatsappbusiness.com/policy/) before implementing or operating this path; a template approval alone does not authorize every recipient or use case.

The adapter would also need owner authorization, bounded client scopes, durable send IDs, and authenticated status webhooks. Server acceptance, delivery, and read status must remain distinct. Existing archive read permissions must not silently grant a new business account permission to send.

## Can the same number keep using the Business app?

Meta has announced that businesses can use the WhatsApp Business app and Business Platform together. See the [official coexistence announcement](https://about.fb.com/news/2025/09/bringing-new-tools-to-help-businesses-boost-engagement-customer-support-and-discoverability/).

Eligibility and onboarding details need investigation for a supported integration. Do not assume every account, country, or number qualifies, or that linking this app establishes coexistence. Cloud API credentials remain separate from the QR-linked session. These references do not establish Cloud API group support or access to a user's complete personal message history.

For this project's supported workflow, use the existing [QR setup](setup.md) and [outbound permissions](clients/outbound-messaging.md).
