/** @type {import('tailwindcss').Config} */
export default {
  content: ['./index.html', './src/**/*.{js,ts,jsx,tsx}'],
  theme: {
    extend: {
      // AllMyStuff design language — colours + font only, no layout changes.
      // The UI leans hard on `neutral-*` (≈280 usages), so re-tinting that ramp
      // toward AllMyStuff's violet-grey (hue 285) shifts the whole surface/ink
      // palette in one place without touching a single component.
      colors: {
        neutral: {
          50: '#f9f9ff',
          100: '#f4f4fd',
          200: '#e4e4ed',
          300: '#d2d3e1',
          400: '#9f9fad',
          500: '#72727f',
          600: '#51515d',
          700: '#3e3e4d',
          800: '#252532',
          900: '#161622',
          950: '#090914'
        },
        accent: {
          DEFAULT: '#f11ea1',
          soft: '#ff77c2'
        }
      },
      fontFamily: {
        sans: [
          'Inter Variable',
          'Inter',
          '-apple-system',
          'BlinkMacSystemFont',
          'Segoe UI',
          'Roboto',
          'system-ui',
          'sans-serif'
        ]
      }
    }
  },
  plugins: [],
  corePlugins: {
    preflight: false
  }
};
