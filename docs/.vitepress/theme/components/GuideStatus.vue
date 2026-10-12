<script setup lang="ts">
import { computed } from 'vue';
import { useData } from 'vitepress';
import { withBase } from 'vitepress';
import { guideNotice } from '../../scripts/guide-notice.mjs';

const { frontmatter, page } = useData();
const notice = computed(() => frontmatter.value.qualification
  ? guideNotice(page.value.relativePath.replace(/\.md$/, ''), frontmatter.value.qualification)
  : null);
</script>

<template>
  <section v-if="notice" class="guide-status" aria-label="Setup status">
    <strong>{{ notice.title }}</strong>
    <p>{{ notice.text }} <a :href="withBase('/compatibility.html')">See compatibility.</a></p>
  </section>
</template>
