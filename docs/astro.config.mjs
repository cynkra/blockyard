// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

// https://astro.build/config
export default defineConfig({
	integrations: [
		starlight({
			title: 'Blockyard',
			social: [{ icon: 'github', label: 'GitHub', href: 'https://github.com/a2-ai/blockyard' }],
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
					],
				},
				{
					label: 'Reference',
					items: [
						{ label: 'REST API', slug: 'reference/api' },
						{ label: 'Configuration File', slug: 'reference/config' },
					],
				},
			],
		}),
	],
});
