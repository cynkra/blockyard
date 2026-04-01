// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import { visit } from 'unist-util-visit';

const BASE = process.env.DOCS_BASE || '/blockyard';

/** Rehype plugin: prepend base path to internal links in Markdown content. */
function rehypeBaseLinks() {
	return (tree) => {
		visit(tree, 'element', (node) => {
			if (node.tagName === 'a' && typeof node.properties.href === 'string') {
				const href = node.properties.href;
				if (href.startsWith('/') && !href.startsWith(BASE + '/')) {
					node.properties.href = BASE + href;
				}
			}
		});
	};
}

// https://astro.build/config
export default defineConfig({
	site: 'https://cynkra.github.io',
	base: BASE,
	markdown: {
		rehypePlugins: [rehypeBaseLinks],
	},
	integrations: [
		starlight({
			title: 'Blockyard',
			customCss: ['./src/styles/custom.css'],
			social: [{ icon: 'github', label: 'GitHub', href: 'https://github.com/cynkra/blockyard' }],
			sidebar: [
				{
					label: 'Getting Started',
					items: [
						{ label: 'What is Blockyard?', slug: 'getting-started/overview' },
						{ label: 'Installation', slug: 'getting-started/installation' },
						{ label: 'Quick Start', slug: 'getting-started/quickstart' },
					],
				},
				{
					label: 'Guides',
					items: [
						{ label: 'Deploying an App', slug: 'guides/deploying' },
						{ label: 'Configuration', slug: 'guides/configuration' },
						{ label: 'Authorization', slug: 'guides/authorization' },
						{ label: 'Credential Management', slug: 'guides/credentials' },
					{ label: 'Observability', slug: 'guides/observability' },
					],
				},
				{
					label: 'Reference',
					items: [
						{ label: 'CLI', slug: 'reference/cli' },
						{ label: 'REST API', slug: 'reference/api' },
						{ label: 'Configuration File', slug: 'reference/config' },
					],
				},
			],
		}),
	],
});
