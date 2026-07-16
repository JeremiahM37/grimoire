import logging

import uvicorn

from . import config
from .app import create_app

logging.basicConfig(level=logging.INFO,
                    format="%(asctime)s %(name)s %(levelname)s %(message)s")

if __name__ == "__main__":
    uvicorn.run(create_app(), host=config.HOST, port=config.PORT, log_level="info")
