import { h, nextTick } from 'vue';
import type { Theme } from 'vitepress';
import DefaultTheme from 'vitepress/theme-without-fonts';
import GuideStatus from './components/GuideStatus.vue';
import PageMarkdown from './components/PageMarkdown.vue';
import './custom.css';
import { pages } from '../public-pages.mjs';

export default {
  extends: DefaultTheme,
  enhanceApp({ router, siteData }) {
    if (typeof window === 'undefined') return;
    const previous = router.onAfterRouteChange;
    router.onAfterRouteChange = async href => {
      await previous?.(href);
      // VitePress 1.6 rewrites index.html to a directory after loading a page.
      // Restore the reviewed explicit route after that scheduled update, also
      // on hydration and back/forward navigation. No server rewrite is needed.
      await nextTick();
      const route = router.route.data.relativePath.replace(/\.md$/, '');
      if (pages.some(page => page.route === route) && /(?:^|\/)index$/.test(route)) {
        const url = new URL(location.href);
        url.pathname = `${siteData.value.base}${route}.html`;
        history.replaceState(history.state, '', url);
      }
    };
  },
  Layout() {
    return h(DefaultTheme.Layout, null, {
      'doc-before': () => h(GuideStatus),
      'doc-after': () => h(PageMarkdown)
    });
  }
} satisfies Theme;
