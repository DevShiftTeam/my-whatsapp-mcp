import { defineConfig } from 'vitepress';
import { pages } from './public-pages.mjs';
import { resolveSite } from './scripts/site.mjs';
import { canonicalURL } from './scripts/urls.mjs';
import { explicitMarkdownRoutes, searchField } from './scripts/explicit-routes.mjs';

const site = resolveSite();
const sections = [...new Set(pages.map(page => page.section))].map(text => ({
  text, items: pages.filter(page => page.section === text).map(page => ({ text: page.title, link: `/${page.route}.html` }))
}));

export default defineConfig({
  title: 'My WhatsApp',
  description: 'A private WhatsApp archive with a local browser interface.',
  base: site.base,
  srcDir: '.vitepress/.content',
  cleanUrls: false,
  appearance: true,
  lastUpdated: false,
  vite: { publicDir: false },
  markdown: { config: explicitMarkdownRoutes },
  themeConfig: {
    logo: '/assets/logo.png',
    search: { provider: 'local', options: { miniSearch: { options: { extractField: searchField } } } },
    outline: [2, 3],
    nav: sections.map(section => ({ text: section.text, items: section.items })),
    sidebar: sections,
    socialLinks: [{ icon: 'github', link: site.repositoryURL, ariaLabel: 'My WhatsApp on GitHub' }],
    footer: { message: 'Your archive stays with you. These pages explain the current qualification.' }
  },
  transformHead({ pageData }) {
    const route = pageData.relativePath.replace(/\.md$/, '');
    return pages.some(page => page.route === route)
      ? [['link', { rel: 'canonical', href: canonicalURL(route, site.baseURL) }]] : [];
  }
});
