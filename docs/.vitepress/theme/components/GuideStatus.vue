<script setup lang="ts">
import { computed } from 'vue';
import { useData } from 'vitepress';

const { frontmatter } = useData();
const qualification = computed(() => frontmatter.value.qualification);
const availability = { planned: 'Planned', preview: 'Preview', available: 'Available' };
const statuses = { documentation_reviewed: 'Documentation reviewed', locally_tested: 'Locally tested', end_to_end_verified: 'End-to-end verified' };
</script>

<template>
  <section v-if="qualification" class="guide-status" aria-label="Guide qualification">
    <div class="guide-status__summary">
      <strong>{{ availability[qualification.availability as keyof typeof availability] }}</strong>
      <span>{{ statuses[qualification.status as keyof typeof statuses] }}</span>
    </div>
    <p>Documentation reviewed <time :datetime="qualification.reviewedAt">{{ qualification.reviewedAt }}</time>.</p>
    <details>
      <summary>Qualification record</summary>
      <p v-if="qualification.tests.length === 0">No runtime tests are recorded for this guide.</p>
      <pre><code>{{ JSON.stringify(qualification, null, 2) }}</code></pre>
    </details>
  </section>
</template>
