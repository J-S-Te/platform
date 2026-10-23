#!/usr/bin/env python3
from pathlib import Path
import unittest
ROOT=Path(__file__).resolve().parents[4]
URL='http://project-api:8082/internal/v1/dashboard/projects/snapshot'
class ProjectionAssetsTests(unittest.TestCase):
    def test_api_only_projection_has_explicit_independent_url_everywhere(self):
        for name in ['data_analysis/.env.example','data_analysis/compose.yaml','platform/compose.local.yaml','platform/deploy/production/subsystems.d/data-analysis-prod.yaml','platform/deploy/production/subsystem-templates/data-analysis.env.example']:
            with self.subTest(name=name):
                text=(ROOT/name).read_text()
                self.assertIn('PROJECT_PROJECTION_URL',text)
                self.assertIn(URL,text)
                self.assertIn('PROJECT_INTERNAL_URL',text)
    def test_templates_expose_bounded_projection_reader(self):
        for name in ['data_analysis/.env.example','platform/deploy/production/subsystem-templates/data-analysis.env.example']:
            text=(ROOT/name).read_text()
            for item in ['PROJECT_PROJECTION_HTTP_TIMEOUT=35s','PROJECT_PROJECTION_MAX_PROJECTS=50000','PROJECT_PROJECTION_MAX_BYTES=67108864','PROJECT_PROJECTION_MAX_LINE_BYTES=65536']:
                self.assertIn(item,text)
if __name__=='__main__':unittest.main()
