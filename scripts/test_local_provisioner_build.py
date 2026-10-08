"""Guard local helper build routing without weakening its network/socket boundary."""
from pathlib import Path
import unittest


class LocalProvisionerBuildTest(unittest.TestCase):
    def test_daemon_build_is_scoped_to_local_helper(self):
        root = Path(__file__).resolve().parents[1]
        compose = (root / 'compose.local.yaml').read_text()
        helper = compose.split('\n  subsystem-provisioner:\n', 1)[1].split('\n  worker:\n', 1)[0]
        self.assertIn('DOCKER_BUILDKIT: ${PROVISIONER_DOCKER_BUILDKIT:-0}', helper)
        self.assertIn('networks: [docker-control]', helper)
        self.assertIn('DOCKER_HOST: tcp://docker-socket-proxy:2375', helper)
        self.assertNotIn('/var/run/docker.sock:', helper)
        self.assertIn('read_only: true', helper)
        self.assertEqual(compose.count('PROVISIONER_DOCKER_BUILDKIT'), 1)


if __name__ == '__main__':
    unittest.main()
