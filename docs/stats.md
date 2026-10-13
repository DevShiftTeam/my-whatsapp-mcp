# Stats

The app opens on **Stats**. Stats and **Archive** use **All saved messages**: current messages plus eligible captured history when configured. Archive opens recent messages before you search; clearing the search returns to browsing. Stats refreshes every five seconds while that view and browser tab are visible.

## Appearance

Open **Appearance** in the header and choose **System**, **Light**, or **Dark** from the styled options. The compact mobile header uses an Appearance icon. System is the default and updates when your device's setting changes. Your choice is saved in this browser for this app address, including the login screen, without changing other devices or archive settings. Charts, tables, filters, and dialogs use the same theme on desktop and mobile. See [local setup](setup.md#choose-light-or-dark) for details.

## Choose what to count

Choose **Date range**, **Chat**, and **Message type** to filter saved-message counts, charts, highlights, and breakdowns together. Date ranges include the last 7, 30, or 90 days, **All saved history**, and **Custom dates**. Custom dates include both selected local calendar days. Message types are **All message types**, **Text**, **Images**, **Voice notes**, and **Other**.

## Explore saved activity

**Message activity** groups matching messages into UTC day, week, month, or year buckets based on their span. Choose **Bars** or **Line**, then point to or select a period to see its count. With the chart focused, use Left and Right arrow keys to explore. The activity table provides the same counts as text.

**Your messaging rhythm** shows weekday and hour in UTC. Select an hour to see its count, or use the arrow keys to move between hours and weekdays. A text table shows weekday totals. These chart times can differ from the local calendar days used by the date filter.

Highlights describe the filtered saved messages:

- **Days with a conversation** counts UTC calendar days with at least one readable message.
- **Longest daily streak** counts consecutive active UTC days.
- **Busiest day** identifies the UTC day with the most messages.
- **Messages per active day** divides messages by active days, excluding days without readable saved messages.

Missing history can create gaps in these views. A quiet day here does not establish that you were inactive on WhatsApp.

## Browse conversations and people

Under **People and conversations**, switch between **Chats** and **People**. People are archived message senders, including you, rather than your phone's contact list. Missing names use an explicit unnamed label.

Rows show message totals, sent and received counts, and the time of the latest saved message. People also show how many chats they contributed to. **Last active** refers to that saved-message time, not a person's online status.

Sort by **Most messages**, **Name**, or **Recently active**. Choose 10, 25, or 50 rows per page and use **Previous** and **Next** to browse the full filtered breakdown. Changing filters, breakdown, sort, or page size returns to the first page. Automatic refresh keeps your current page, moving to the last available page if the filtered results shrink. Counts and rankings can change as the archive updates.

## Check processing and coverage

One compact coverage notice summarizes incomplete history. Open **Details** to see the specific limitations, including delivered or captured history, accepted unknown records, snapshot recovery, and unavailable or conflicting messages. These are coverage warnings, not a claim that the service is offline.

**Processing** reports work for the full archive and stays independent of these filters. **Voice notes with text** measures how many filtered saved voice notes have generated text; it does not measure recognition accuracy or complete WhatsApp history.

Stats counts the permitted saved view once, including eligible captured-only messages. A current copy takes precedence over its captured copy. Current archive choices and known deletion, edit, expiry and view-once restrictions apply to captured history too. Captured originals do not add current downloads or processing jobs.

Stats reflects readable saved data within your archive choices. WhatsApp may have delivered only part of your history. Updates happen in place with a small loading indicator; the page does not reload. During import, refresh keeps the last successful counts until a new guarded read succeeds. Busy, oversized, timed-out or temporarily unreachable reads leave those counts visible with an explanation; narrow the filters if the view is too large. Permission changes, source failures or an ended owner session clear withheld data. Use **Refresh stats** to retry other failed updates. Remote access uses the same password-protected admin setup described in [MCP and remote access](mcp.md).

If import status briefly cannot be refreshed, the app labels the last confirmed progress and checks again without implying that import is still active. The activity spinner stays off until fresh status confirms work. Transient Stats updates keep the last successful counts with calm feedback; actual permission or source failures remain errors.

Normal syncing, older-history import and media processing do not require Stats or MCP to stop. An archive-safety pause is different: the app cannot verify that protected reads are still safe, for example after an unresolved source change or interrupted write. It withholds those reads rather than showing potentially deleted or unauthorized data. Saved files and existing connections are retained; resetting the archive or replacing credentials is not the ordinary remedy.

An explicit `archive_unavailable` response is treated as an archive/source failure, not a routine connection timeout. It can hide the previous view until a safe read succeeds. A gateway error or interrupted connection without that source-failure signal keeps the last successful counts visible.

Charts and aggregates run locally without a chart service, model, or additional setup.

For first-time linking and import checks, see [local setup](setup.md). For voice processing, see [transcription](transcription.md).
