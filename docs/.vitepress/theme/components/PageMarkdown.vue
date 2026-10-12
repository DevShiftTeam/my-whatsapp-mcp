<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue';
import { useData, withBase } from 'vitepress';
import { pages } from '../../public-pages.mjs';
import { markdownURL } from '../../scripts/urls.mjs';

const { page } = useData();
const route = computed(() => page.value.relativePath.replace(/\.md$/, ''));
const publicPage = computed(() => pages.some(entry => entry.route === route.value));
const url = computed(() => publicPage.value ? withBase(markdownURL(route.value, '/')) : '');
const filename = computed(() => `wa-indexer-${route.value.replaceAll('/', '-')}.md`);
const loading = ref(false);
const status = ref('');
const markdown = ref('');
const manualCopy = ref(false);
const textarea = ref<HTMLTextAreaElement>();
let operation = 0;
watch(route, () => {
  operation++;
  loading.value = false;
  status.value = '';
  markdown.value = '';
  manualCopy.value = false;
});

async function selectMarkdown() {
  await nextTick();
  textarea.value?.focus();
  textarea.value?.select();
}

async function copyMarkdown() {
  if (loading.value || !publicPage.value) return;
  const current = ++operation;
  loading.value = true;
  status.value = 'Loading Markdown…';
  try {
    const response = await fetch(url.value, { credentials: 'same-origin' });
    if (!response.ok) throw new Error('Markdown unavailable');
    const text = await response.text();
    if (current !== operation) return;
    markdown.value = text;
    try {
      await navigator.clipboard.writeText(text);
      if (current !== operation) return;
      manualCopy.value = false;
      status.value = 'Markdown copied.';
    } catch {
      if (current !== operation) return;
      manualCopy.value = true;
      status.value = 'Clipboard access is unavailable. Copy the selected Markdown text below.';
      await selectMarkdown();
    }
  } catch {
    if (current === operation) status.value = 'Could not load Markdown. Try again or use View Markdown.';
  } finally {
    if (current === operation) loading.value = false;
  }
}
</script>

<template>
  <section v-if="publicPage" class="page-markdown" aria-labelledby="page-markdown-heading">
    <h2 id="page-markdown-heading">Save this guide</h2>
    <p>Download or copy this page as Markdown for offline use.</p>
    <div class="page-markdown__actions">
      <a :href="url" target="_self">View Markdown</a>
      <a :href="url" :download="filename">Download Markdown</a>
      <button type="button" :disabled="loading" @click="copyMarkdown">Copy page as Markdown</button>
    </div>
    <p class="page-markdown__status" role="status" aria-live="polite" aria-atomic="true">{{ status }}</p>
    <div v-if="manualCopy" class="page-markdown__fallback">
      <label for="page-markdown-text">Page Markdown for manual copying</label>
      <textarea id="page-markdown-text" ref="textarea" :value="markdown" readonly rows="10" spellcheck="false" />
      <button type="button" @click="selectMarkdown">Select Markdown text</button>
    </div>
  </section>
</template>
